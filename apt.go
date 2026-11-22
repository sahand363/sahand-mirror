package main

// apt caching proxy. Target servers send their http:// apt traffic through the
// hub (Acquire::http::Proxy). While the hub is online it fetches from the
// real mirror and caches; when upstream fails it serves the cache. Files of
// the main Ubuntu/Debian archives are cached under a "family" key so that
// de.archive.ubuntu.com, archive.ubuntu.com and an Iranian mirror share one
// cache. A prefetch job resolves packages + dependencies from Packages.gz and
// warms the cache for chosen releases/architectures.

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// families of well-known archives and their default upstreams
var aptFamilies = []string{"ubuntu", "ubuntu-ports", "debian", "debian-security"}

var defaultAptUpstream = map[string]string{
	"ubuntu":          "http://archive.ubuntu.com/ubuntu",
	"ubuntu-ports":    "http://ports.ubuntu.com/ubuntu-ports",
	"debian":          "http://deb.debian.org/debian",
	"debian-security": "http://deb.debian.org/debian-security",
}

// release targets offered for prefetch
var aptReleases = []struct{ Distro, Suite string }{
	{"ubuntu", "jammy"}, {"ubuntu", "noble"}, {"debian", "bookworm"}, {"debian", "trixie"},
}

func defaultAptTargets() []string {
	var out []string
	for _, r := range aptReleases {
		out = append(out, r.Distro+"/"+r.Suite)
	}
	return out
}

type AptTarget struct {
	Distro string // ubuntu | debian
	Suite  string // noble
	Arch   string // deb arch: amd64 arm64 armhf i386
}

func (t AptTarget) Key() string { return t.Distro + "/" + t.Suite + "/" + t.Arch }

func debArch(a string) string {
	switch a {
	case "armv7":
		return "armhf"
	case "386":
		return "i386"
	case "amd64", "arm64":
		return a
	}
	return ""
}

// aptTargetsFor: enabled releases × the app's architectures (amd64+arm64 when unset).
func aptTargetsFor(cfg Config, archs []string) []AptTarget {
	rels := cfg.AptTargets
	if rels == nil {
		rels = defaultAptTargets()
	}
	var das []string
	for _, a := range archs {
		if d := debArch(a); d != "" && !contains(das, d) {
			das = append(das, d)
		}
	}
	if len(das) == 0 {
		das = []string{"amd64", "arm64"}
	}
	var out []AptTarget
	for _, r := range rels {
		d, s, ok := strings.Cut(r, "/")
		if !ok {
			continue
		}
		for _, a := range das {
			if d == "debian" && a == "i386" && s == "trixie" {
				continue
			}
			out = append(out, AptTarget{d, s, a})
		}
	}
	return out
}

// AptState is persisted in state.json.
type AptState struct {
	Targets  map[string]*AptTargetState `json:"targets"`
	Running  bool                       `json:"running"`
	Progress string                     `json:"progress,omitempty"`
	LastErr  string                     `json:"last_error,omitempty"`
	LastRun  time.Time                  `json:"last_run"`
}

type AptTargetState struct {
	Packages map[string]string `json:"packages"` // requested name -> resolved "name=version"
	Missing  []string          `json:"missing,omitempty"`
	Files    int               `json:"files"`
	Bytes    int64             `json:"bytes"`
	At       time.Time         `json:"at"`
	Error    string            `json:"error,omitempty"`
}

func aptMissing(st *State, targets []AptTarget, pkg string) []string {
	var miss []string
	for _, t := range targets {
		ts := (*AptTargetState)(nil)
		if st.Apt != nil {
			ts = st.Apt.Targets[t.Key()]
		}
		if ts == nil || ts.Packages[pkg] == "" {
			miss = append(miss, t.Key())
		}
	}
	return miss
}

// ---------- cache keys ----------

var aptFamilyHostRe = map[string]*regexp.Regexp{
	"ubuntu":          regexp.MustCompile(`^([a-z0-9-]+\.)?(archive|security)\.ubuntu\.com$`),
	"ubuntu-ports":    regexp.MustCompile(`^([a-z0-9-]+\.)?ports\.ubuntu\.com$`),
	"debian":          regexp.MustCompile(`^(deb|ftp(\.[a-z0-9-]+)?|httpredir|cdn-fastly\.deb|ftp\.[a-z]{2})\.debian\.org$`),
	"debian-security": regexp.MustCompile(`^(security|deb|security-cdn)\.debian\.org$`),
}

var aptRoots = map[string]string{"/ubuntu": "ubuntu", "/ubuntu-ports": "ubuntu-ports", "/debian": "debian", "/debian-security": "debian-security"}

// aptKey maps an apt URL to its cache key: "<family>/<rest>" for the main
// archives (host-independent), else "_hosts/<host>/<path>".
func (s *Server) aptKey(u *url.URL) string {
	p := path.Clean("/" + u.Path)
	host := strings.ToLower(u.Hostname())
	extra := s.aptExtraHosts()
	for root, fam := range aptRoots {
		if !strings.HasPrefix(p, root+"/") {
			continue
		}
		known := aptFamilyHostRe[fam].MatchString(host) || extra[host]
		if fam == "debian" && host == "security.debian.org" {
			known = false
		}
		if known {
			return fam + p[len(root):]
		}
	}
	if host == "security.debian.org" && strings.HasPrefix(p, "/dists/") || host == "security.debian.org" && strings.HasPrefix(p, "/pool/") {
		return "debian-security" + p // old style security.debian.org/ without root
	}
	return "_hosts/" + host + p
}

// hosts of configured upstreams/internal mirrors share the family cache
func (s *Server) aptExtraHosts() map[string]bool {
	c := s.cfg.Get()
	m := map[string]bool{}
	for _, mp := range []map[string]string{c.AptUpstream, c.AptInternal} {
		for _, v := range mp {
			if pu, err := url.Parse(v); err == nil && pu.Hostname() != "" {
				m[strings.ToLower(pu.Hostname())] = true
			}
		}
	}
	return m
}

func (s *Server) aptUpstream(fam string) string {
	if v := strings.TrimRight(s.cfg.Get().AptUpstream[fam], "/"); v != "" {
		return v
	}
	return defaultAptUpstream[fam]
}

func aptImmutable(p string) bool {
	return strings.Contains(p, "/pool/") || strings.Contains(p, "/by-hash/") || strings.HasSuffix(p, ".deb") || strings.HasSuffix(p, ".udeb")
}

func aptAllowed(p string) bool {
	return strings.Contains(p, "/dists/") || strings.Contains(p, "/pool/") || strings.HasSuffix(p, ".deb")
}

var byHashRe = regexp.MustCompile(`/by-hash/SHA256/([0-9a-f]{64})$`)

func (s *Server) aptCachePath(key string) (string, error) {
	return safeJoin(filepath.Join(s.data, "apt"), filepath.FromSlash(key))
}

// candidates: the requested URL, then the family's configured upstream,
// then the internal (e.g. Iranian) mirror.
func (s *Server) aptCandidates(u *url.URL, key string) []string {
	out := []string{u.String()}
	fam, rest, _ := strings.Cut(key, "/")
	if fam == "_hosts" {
		return out
	}
	rest = "/" + rest
	if up := s.aptUpstream(fam); up != "" && up+rest != u.String() {
		out = append(out, up+rest)
	}
	if in := strings.TrimRight(s.cfg.Get().AptInternal[fam], "/"); in != "" {
		out = append(out, in+rest)
	}
	return out
}

var aptClient = func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.ResponseHeaderTimeout = 25 * time.Second
	tr.MaxIdleConnsPerHost = 8
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}()

// aptFetch tries the candidates in order. A 404 from a reachable upstream is
// final (errNotFound); network errors and 5xx move on to the next one.
var errNotFound = errors.New("not found upstream")

func aptOpen(ctx context.Context, cands []string) (*http.Response, string, error) {
	var last error
	for _, c := range cands {
		req, err := http.NewRequestWithContext(ctx, "GET", c, nil)
		if err != nil {
			last = err
			continue
		}
		req.Header.Set("User-Agent", "sahand-mirror/"+version+" (apt cache)")
		resp, err := aptClient.Do(req)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode == 200 {
			return resp, c, nil
		}
		resp.Body.Close()
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			return nil, c, errNotFound
		}
		last = fmt.Errorf("%s: HTTP %d", c, resp.StatusCode)
	}
	if last == nil {
		last = errors.New("no upstream")
	}
	return nil, "", last
}

// ---------- proxy handler ----------

func (s *Server) aptAuthorized(r *http.Request) bool {
	key := s.cfg.Get().AccessKey
	if key == "" {
		return true
	}
	if h := r.Header.Get("Proxy-Authorization"); strings.HasPrefix(h, "Basic ") {
		r2 := &http.Request{Header: http.Header{"Authorization": {h}}}
		if _, pw, ok := r2.BasicAuth(); ok && pw == key {
			return true
		}
	}
	if _, pw, ok := r.BasicAuth(); ok && pw == key {
		return true
	}
	return r.Header.Get("X-Mirror-Key") == key || r.URL.Query().Get("k") == key
}

// handleProxy serves absolute-URI proxy requests (GET http://host/path).
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, "sahand-mirror: only http:// apt repositories can be proxied (https cannot be cached)", http.StatusMethodNotAllowed)
		return
	}
	if !s.aptAuthorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="sahand-mirror"`)
		http.Error(w, "sahand-mirror: access key required (http://sm:KEY@hub/)", http.StatusProxyAuthRequired)
		return
	}
	if s.cfg.Get().AptProxyOff {
		http.Error(w, "sahand-mirror: apt proxy is disabled in settings", http.StatusForbidden)
		return
	}
	u := *r.URL
	u.RawQuery = ""
	s.serveApt(w, r, &u)
}

// handleAptPath serves /apt/<host>/<path> for servers whose sources.list
// points at the hub directly.
func (s *Server) handleAptPath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apt/")
	host, p, ok := strings.Cut(rest, "/")
	if !ok || host == "" || strings.ContainsAny(host, "@ ") {
		http.Error(w, "usage: /apt/<host>/<path>", 400)
		return
	}
	if s.cfg.Get().AptProxyOff {
		http.Error(w, "sahand-mirror: apt proxy is disabled in settings", http.StatusForbidden)
		return
	}
	s.serveApt(w, r, &url.URL{Scheme: "http", Host: host, Path: "/" + p})
}

func (s *Server) serveApt(w http.ResponseWriter, r *http.Request, u *url.URL) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "GET only", 405)
		return
	}
	if u.Scheme != "http" || u.Host == "" || !aptAllowed(u.Path) || strings.Contains(u.Path, "..") {
		http.Error(w, "sahand-mirror: only apt repository paths (dists/, pool/, *.deb) are proxied", http.StatusForbidden)
		return
	}
	key := s.aptKey(u)
	cp, err := s.aptCachePath(key)
	if err != nil {
		http.Error(w, "bad path", 400)
		return
	}
	fi, statErr := os.Stat(cp)
	cached := statErr == nil && fi.Mode().IsRegular()
	immutable := aptImmutable(u.Path)
	logHit := func(src string) { log.Printf("apt %s %s %s %s", clientIP(r), src, u.Host, u.Path) }
	if cached && (immutable || time.Since(fi.ModTime()) < 5*time.Minute) {
		logHit("cache")
		w.Header().Set("X-Sahand-Mirror", "cache")
		http.ServeFile(w, r, cp)
		return
	}
	ctx := r.Context()
	resp, from, err := aptOpen(ctx, s.aptCandidates(u, key))
	if err != nil {
		if errors.Is(err, errNotFound) {
			logHit("404")
			http.Error(w, "not found", 404)
			return
		}
		if cached {
			logHit("stale")
			w.Header().Set("X-Sahand-Mirror", "stale-cache")
			http.ServeFile(w, r, cp)
			return
		}
		logHit("miss-offline")
		http.Error(w, "sahand-mirror: upstream unreachable and not cached: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	logHit("upstream " + from)
	if r.Method == "HEAD" {
		w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
		w.WriteHeader(200)
		return
	}
	// stream to client and tee into the cache
	w.Header().Set("X-Sahand-Mirror", "upstream")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		w.Header().Set("Last-Modified", lm)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	tmp, terr := s.aptTemp(cp)
	if terr != nil {
		io.Copy(w, resp.Body)
		return
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(tmp, h), io.TeeReader(resp.Body, w))
	s.aptCommit(tmp, cp, n, resp.ContentLength, hex.EncodeToString(h.Sum(nil)), u.Path, cerr)
}

func (s *Server) aptTemp(cp string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(cp), 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(filepath.Dir(cp), ".tmp-*")
}

// aptCommit renames a finished download into the cache when it is complete
// (and matches its by-hash name).
func (s *Server) aptCommit(tmp *os.File, cp string, n, want int64, sum, p string, err error) error {
	name := tmp.Name()
	defer os.Remove(name)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if want >= 0 && n != want {
		return fmt.Errorf("short download %d/%d", n, want)
	}
	if m := byHashRe.FindStringSubmatch(p); m != nil && m[1] != sum {
		return errors.New("by-hash checksum mismatch")
	}
	os.Chmod(name, 0o644)
	return os.Rename(name, cp)
}

// aptGet fetches a URL (by family key) into the cache and returns its path.
// refresh=false uses an existing cached copy.
func (s *Server) aptGet(ctx context.Context, fam, rest string, refresh bool, wantSHA string) (string, int64, error) {
	key := fam + "/" + strings.TrimPrefix(rest, "/")
	cp, err := s.aptCachePath(key)
	if err != nil {
		return "", 0, err
	}
	if fi, err := os.Stat(cp); err == nil && !refresh {
		return cp, fi.Size(), nil
	}
	up := s.aptUpstream(fam)
	pu, err := url.Parse(up + "/" + strings.TrimPrefix(rest, "/"))
	if err != nil {
		return "", 0, err
	}
	cands := s.aptCandidates(pu, key)
	resp, _, err := aptOpen(ctx, cands)
	if err != nil {
		if fi, serr := os.Stat(cp); serr == nil {
			return cp, fi.Size(), nil // offline: keep using the cached copy
		}
		return "", 0, fmt.Errorf("%s: %w", pu, err)
	}
	defer resp.Body.Close()
	tmp, err := s.aptTemp(cp)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	sum := hex.EncodeToString(h.Sum(nil))
	if cerr == nil && wantSHA != "" && sum != wantSHA {
		cerr = fmt.Errorf("%s: sha256 mismatch", pu)
	}
	if err := s.aptCommit(tmp, cp, n, resp.ContentLength, sum, pu.Path, cerr); err != nil {
		return "", 0, err
	}
	return cp, n, nil
}

// aptLink stores a copy of a cached file under another key (by-hash name).
func (s *Server) aptLink(src, fam, rest string) {
	dst, err := s.aptCachePath(fam + "/" + strings.TrimPrefix(rest, "/"))
	if err != nil || fileExists(dst) {
		return
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if os.Link(src, dst) != nil {
		if b, err := os.ReadFile(src); err == nil {
			writeFileAtomic(dst, b, 0o644)
		}
	}
}

// ---------- Packages index parsing & dependency resolution ----------

type debPkg struct {
	Name, Version, Filename, SHA256 string
	Size                            int64
	Depends, PreDepends, Provides   string
}

// parsePackages reads a (decompressed) Packages file.
func parsePackages(r io.Reader, fn func(debPkg)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var p debPkg
	var last string
	flush := func() {
		if p.Name != "" {
			fn(p)
		}
		p, last = debPkg{}, ""
	}
	for sc.Scan() {
		l := sc.Text()
		if l == "" {
			flush()
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			if last == "Depends" {
				p.Depends += " " + strings.TrimSpace(l)
			}
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		last = k
		switch k {
		case "Package":
			p.Name = v
		case "Version":
			p.Version = v
		case "Filename":
			p.Filename = v
		case "SHA256":
			p.SHA256 = v
		case "Size":
			fmt.Sscan(v, &p.Size)
		case "Depends":
			p.Depends = v
		case "Pre-Depends":
			p.PreDepends = v
		case "Provides":
			p.Provides = v
		}
	}
	flush()
	return sc.Err()
}

type debIndex struct {
	pkgs     map[string]debPkg
	provides map[string][]string
	fam      map[string]string // package -> family it came from
}

func newDebIndex() *debIndex {
	return &debIndex{pkgs: map[string]debPkg{}, provides: map[string][]string{}, fam: map[string]string{}}
}

// add keeps the highest version of each package (what apt would install).
func (ix *debIndex) add(p debPkg, fam string) {
	if old, ok := ix.pkgs[p.Name]; ok && debVerCmp(old.Version, p.Version) >= 0 {
		return
	}
	ix.pkgs[p.Name] = p
	ix.fam[p.Name] = fam
	for _, v := range splitDeps(p.Provides) {
		if !contains(ix.provides[v[0]], p.Name) {
			ix.provides[v[0]] = append(ix.provides[v[0]], p.Name)
		}
	}
}

var depStripRe = regexp.MustCompile(`\([^)]*\)|\[[^\]]*\]|<[^>]*>`)

// splitDeps parses "a (>= 1) | b, c:any" into [[a b] [c]].
func splitDeps(s string) [][]string {
	var out [][]string
	for _, grp := range strings.Split(depStripRe.ReplaceAllString(s, ""), ",") {
		var alts []string
		for _, a := range strings.Split(grp, "|") {
			a = strings.TrimSpace(a)
			if i := strings.Index(a, ":"); i > 0 {
				a = a[:i]
			}
			if a != "" {
				alts = append(alts, a)
			}
		}
		if len(alts) > 0 {
			out = append(out, alts)
		}
	}
	return out
}

// lookup finds a real package for a name (directly or via Provides).
func (ix *debIndex) lookup(name string) (debPkg, bool) {
	if p, ok := ix.pkgs[name]; ok {
		return p, true
	}
	if pr := ix.provides[name]; len(pr) > 0 {
		sort.Strings(pr)
		return ix.pkgs[pr[0]], true
	}
	return debPkg{}, false
}

// resolve returns the dependency closure of roots (Depends + Pre-Depends,
// first available alternative, versions ignored) and the roots not found.
func (ix *debIndex) resolve(roots []string) (closure []debPkg, resolved map[string]string, missing []string) {
	resolved = map[string]string{}
	seen := map[string]bool{}
	var queue []string
	for _, r := range roots {
		p, ok := ix.lookup(r)
		if !ok {
			missing = append(missing, r)
			continue
		}
		resolved[r] = p.Name + "=" + p.Version
		queue = append(queue, p.Name)
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if seen[n] {
			continue
		}
		seen[n] = true
		p := ix.pkgs[n]
		closure = append(closure, p)
		for _, grp := range splitDeps(p.PreDepends + "," + p.Depends) {
			pick := ""
			for _, alt := range grp {
				if q, ok := ix.lookup(alt); ok {
					pick = q.Name
					break
				}
			}
			if pick != "" && !seen[pick] {
				queue = append(queue, pick)
			}
		}
	}
	sort.Slice(closure, func(i, j int) bool { return closure[i].Name < closure[j].Name })
	return
}

// debVerCmp compares Debian versions ([epoch:]upstream[-revision]).
func debVerCmp(a, b string) int {
	ea, ra := splitEpoch(a)
	eb, rb := splitEpoch(b)
	if ea != eb {
		if ea < eb {
			return -1
		}
		return 1
	}
	ua, va := splitRev(ra)
	ub, vb := splitRev(rb)
	if c := debStrCmp(ua, ub); c != 0 {
		return c
	}
	return debStrCmp(va, vb)
}

func splitEpoch(v string) (int, string) {
	if i := strings.Index(v, ":"); i > 0 {
		n := 0
		fmt.Sscan(v[:i], &n)
		return n, v[i+1:]
	}
	return 0, v
}

func splitRev(v string) (string, string) {
	if i := strings.LastIndex(v, "-"); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func debOrder(c byte) int {
	switch {
	case c == '~':
		return -1
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		return int(c)
	case c == 0:
		return 0
	}
	return int(c) + 256
}

func debStrCmp(a, b string) int {
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for a != "" || b != "" {
		// non-digit prefix
		for (a != "" && !isDigit(a[0])) || (b != "" && !isDigit(b[0])) {
			var ca, cb byte
			if a != "" && !isDigit(a[0]) {
				ca = a[0]
			}
			if b != "" && !isDigit(b[0]) {
				cb = b[0]
			}
			if oa, ob := debOrder(ca), debOrder(cb); oa != ob {
				if oa < ob {
					return -1
				}
				return 1
			}
			if ca != 0 {
				a = a[1:]
			}
			if cb != 0 {
				b = b[1:]
			}
		}
		// digit run
		i, j := 0, 0
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		na, nb := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
		if len(na) != len(nb) {
			if len(na) < len(nb) {
				return -1
			}
			return 1
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
		a, b = a[i:], b[j:]
	}
	return 0
}

// ---------- release files ----------

type releaseFile struct {
	sums   map[string]string // relative path -> sha256
	sizes  map[string]int64
	byHash bool
}

func parseRelease(b []byte) releaseFile {
	rf := releaseFile{sums: map[string]string{}, sizes: map[string]int64{}}
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "Acquire-By-Hash:") && strings.Contains(l, "yes") {
			rf.byHash = true
		}
		if strings.HasPrefix(l, "SHA256:") {
			in = true
			continue
		}
		if in {
			if !strings.HasPrefix(l, " ") {
				in = false
				continue
			}
			f := strings.Fields(l)
			if len(f) == 3 {
				rf.sums[f[2]] = f[0]
				var n int64
				fmt.Sscan(f[1], &n)
				rf.sizes[f[2]] = n
			}
		}
	}
	return rf
}

// suitesFor lists (family, suite, components) for a target.
func suitesFor(t AptTarget) []struct {
	fam, suite string
	comps      []string
} {
	type sc = struct {
		fam, suite string
		comps      []string
	}
	if t.Distro == "ubuntu" {
		fam := "ubuntu"
		if t.Arch != "amd64" && t.Arch != "i386" {
			fam = "ubuntu-ports"
		}
		c := []string{"main", "universe"}
		return []sc{{fam, t.Suite, c}, {fam, t.Suite + "-updates", c}, {fam, t.Suite + "-security", c}}
	}
	c := []string{"main"}
	return []sc{{"debian", t.Suite, c}, {"debian", t.Suite + "-updates", c}, {"debian-security", t.Suite + "-security", c}}
}

// ---------- prefetch job ----------

func (s *Server) aptProgress(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	s.store.Mutate(func(st *State) {
		if st.Apt == nil {
			st.Apt = &AptState{Targets: map[string]*AptTargetState{}}
		}
		st.Apt.Progress = msg
	})
}

// StartPrefetch runs the prefetch in the background.
func (s *Server) StartPrefetch(pkgs []string, targets []AptTarget) error {
	if !s.aptMu.TryLock() {
		return errBusy
	}
	go func() {
		defer s.aptMu.Unlock()
		if err := s.prefetch(context.Background(), pkgs, targets); err != nil {
			log.Printf("apt prefetch: %v", err)
		}
	}()
	return nil
}

func (s *Server) prefetch(ctx context.Context, pkgs []string, targets []AptTarget) error {
	s.store.Update(func(st *State) {
		if st.Apt == nil {
			st.Apt = &AptState{}
		}
		if st.Apt.Targets == nil {
			st.Apt.Targets = map[string]*AptTargetState{}
		}
		st.Apt.Running, st.Apt.LastRun, st.Apt.LastErr = true, time.Now(), ""
	})
	var errs []string
	for _, t := range targets {
		ts, err := s.prefetchTarget(ctx, t, pkgs)
		if err != nil {
			errs = append(errs, t.Key()+": "+err.Error())
		}
		s.store.Update(func(st *State) {
			old := st.Apt.Targets[t.Key()]
			if ts == nil {
				if old == nil {
					old = &AptTargetState{Packages: map[string]string{}}
					st.Apt.Targets[t.Key()] = old
				}
				old.Error = err.Error()
				return
			}
			if old != nil { // keep packages prefetched earlier for other apps
				for k, v := range old.Packages {
					if _, ok := ts.Packages[k]; !ok {
						ts.Packages[k] = v
					}
				}
			}
			st.Apt.Targets[t.Key()] = ts
		})
	}
	s.store.Update(func(st *State) {
		st.Apt.Running, st.Apt.Progress, st.Apt.LastErr = false, "", strings.Join(errs, "\n")
	})
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *Server) prefetchTarget(ctx context.Context, t AptTarget, pkgs []string) (*AptTargetState, error) {
	ix := newDebIndex()
	var idxErrs []string
	indexes := 0
	for _, sc := range suitesFor(t) {
		s.aptProgress("%s: فهرست %s", t.Key(), sc.suite)
		rel, _, err := s.aptGet(ctx, sc.fam, "dists/"+sc.suite+"/InRelease", true, "")
		if err != nil {
			idxErrs = append(idxErrs, err.Error())
			continue
		}
		rb, _ := os.ReadFile(rel)
		rf := parseRelease(rb)
		for _, comp := range sc.comps {
			base := comp + "/binary-" + t.Arch + "/"
			// keep every flavour apt may ask for (xz first), parse the .gz
			for _, name := range []string{base + "Packages.xz", base + "Packages.gz", comp + "/i18n/Translation-en.xz", comp + "/i18n/Translation-en.gz", comp + "/cnf/Commands-" + t.Arch + ".xz"} {
				sum, ok := rf.sums[name]
				if !ok {
					continue
				}
				dir := path.Dir(name)
				p, _, err := s.aptGet(ctx, sc.fam, "dists/"+sc.suite+"/"+dir+"/by-hash/SHA256/"+sum, false, sum)
				if err != nil {
					if strings.HasSuffix(name, "Packages.gz") {
						idxErrs = append(idxErrs, err.Error())
					}
					continue
				}
				// plain-name copy for clients without by-hash
				dst, _ := s.aptCachePath(sc.fam + "/dists/" + sc.suite + "/" + name)
				os.Remove(dst)
				s.aptLink(p, sc.fam, "dists/"+sc.suite+"/"+name)
				if strings.HasSuffix(name, "Packages.gz") {
					f, err := os.Open(p)
					if err != nil {
						continue
					}
					gz, err := gzip.NewReader(f)
					if err == nil {
						fam := sc.fam
						err = parsePackages(gz, func(p debPkg) { ix.add(p, fam) })
						indexes++
					}
					f.Close()
					if err != nil {
						idxErrs = append(idxErrs, name+": "+err.Error())
					}
				}
			}
		}
	}
	if indexes == 0 {
		return nil, fmt.Errorf("no package index: %s", strings.Join(idxErrs, "; "))
	}
	closure, resolved, missing := ix.resolve(pkgs)
	ts := &AptTargetState{Packages: resolved, Missing: missing, At: time.Now()}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var dlErrs []string
	for i, p := range closure {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.aptProgress("%s: (%d/%d) %s", t.Key(), i+1, len(closure), path.Base(p.Filename))
			_, n, err := s.aptGet(ctx, ix.fam[p.Name], p.Filename, false, p.SHA256)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				dlErrs = append(dlErrs, err.Error())
				return
			}
			ts.Files++
			ts.Bytes += n
		}()
	}
	wg.Wait()
	if len(dlErrs) > 0 {
		ts.Error = fmt.Sprintf("%d file(s) failed: %s", len(dlErrs), dlErrs[0])
		return ts, errors.New(ts.Error)
	}
	if len(idxErrs) > 0 {
		ts.Error = strings.Join(idxErrs, "; ")
	}
	return ts, nil
}

// aptCacheSize returns the bytes used by the apt cache.
func (s *Server) aptCacheSize() int64 { return dirSize(filepath.Join(s.data, "apt")) }
