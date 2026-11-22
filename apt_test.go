package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const packagesFixture = `Package: socat
Version: 1.8.0.0-4build3
Depends: libc6 (>= 2.38), libssl3t64 (>= 3.0.0) | libssl3, libwrap0 (>= 7.6-4~)
Filename: pool/main/s/socat/socat_1.8.0.0-4build3_amd64.deb
Size: 4
SHA256: SUM_SOCAT

Package: libc6
Version: 2.39-0ubuntu8
Pre-Depends: libgcc-s1
Filename: pool/main/g/glibc/libc6_2.39-0ubuntu8_amd64.deb
Size: 4
SHA256: SUM_LIBC6

Package: libgcc-s1
Version: 14-20240412-0ubuntu1
Filename: pool/main/g/gcc-14/libgcc-s1_14-20240412-0ubuntu1_amd64.deb
Size: 4
SHA256: SUM_LIBGCC

Package: libssl3t64
Version: 3.0.13-0ubuntu3
Provides: libssl3 (= 3.0.13-0ubuntu3)
Depends: libc6 (>= 2.34)
Filename: pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3_amd64.deb
Size: 4
SHA256: SUM_LIBSSL

Package: libwrap0
Version: 7.6.q-33
Depends: libc6 (>= 2.33)
Filename: pool/main/t/tcp-wrappers/libwrap0_7.6.q-33_amd64.deb
Size: 4
SHA256: SUM_LIBWRAP

Package: postfix
Version: 3.8.6-1build2
Provides: mail-transport-agent
Filename: pool/main/p/postfix/postfix_3.8.6-1build2_amd64.deb
Size: 4
SHA256: SUM_POSTFIX

`

// security pocket carries a newer libssl3t64 that must win
const securityFixture = `Package: libssl3t64
Version: 3.0.13-0ubuntu3.4
Provides: libssl3 (= 3.0.13-0ubuntu3.4)
Depends: libc6 (>= 2.34)
Filename: pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.4_amd64.deb
Size: 4
SHA256: SUM_LIBSSL_SEC

`

func TestDebVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0}, {"1.0", "1.1", -1}, {"1.10", "1.9", 1}, {"1:0.1", "2.0", 1}, {"1.0~rc1", "1.0", -1},
		{"3.0.13-0ubuntu3", "3.0.13-0ubuntu3.4", -1}, {"2.39-0ubuntu8", "2.39-0ubuntu8.3", -1}, {"1.0-1", "1.0-1", 0},
		{"1.0a", "1.0", 1}, {"1.0+dfsg", "1.0", 1}, {"7.6.q-33", "7.6.q-31", 1},
	}
	for _, c := range cases {
		if got := debVerCmp(c.a, c.b); got != c.want {
			t.Errorf("cmp(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
		if got := debVerCmp(c.b, c.a); got != -c.want {
			t.Errorf("cmp(%s,%s)=%d want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestResolveDeps(t *testing.T) {
	ix := newDebIndex()
	parsePackages(strings.NewReader(packagesFixture), func(p debPkg) { ix.add(p, "ubuntu") })
	parsePackages(strings.NewReader(securityFixture), func(p debPkg) { ix.add(p, "ubuntu") })
	closure, resolved, missing := ix.resolve([]string{"socat", "mail-transport-agent", "no-such-pkg"})
	var names []string
	for _, p := range closure {
		names = append(names, p.Name+"="+p.Version)
	}
	want := "libc6=2.39-0ubuntu8 libgcc-s1=14-20240412-0ubuntu1 libssl3t64=3.0.13-0ubuntu3.4 libwrap0=7.6.q-33 postfix=3.8.6-1build2 socat=1.8.0.0-4build3"
	if strings.Join(names, " ") != want {
		t.Errorf("closure:\n got  %s\n want %s", strings.Join(names, " "), want)
	}
	if resolved["mail-transport-agent"] != "postfix=3.8.6-1build2" || resolved["socat"] == "" {
		t.Errorf("resolved: %v", resolved)
	}
	if len(missing) != 1 || missing[0] != "no-such-pkg" {
		t.Errorf("missing: %v", missing)
	}
	if d := splitDeps("a (>= 1) | b:any, c [amd64] <!nocheck>"); len(d) != 2 || d[0][1] != "b" || d[1][0] != "c" {
		t.Errorf("splitDeps: %v", d)
	}
}

// offlineAptClient makes the apt proxy unable to reach anything but the
// local fake archive, so tests behave the same with or without internet.
func offlineAptClient(t *testing.T) {
	old := aptClient
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			return nil, fmt.Errorf("test: no internet (%s)", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	aptClient = &http.Client{Transport: tr}
	t.Cleanup(func() { aptClient = old })
}

func testServer(t *testing.T, mut func(*Config)) *Server {
	offlineAptClient(t)
	dir := t.TempDir()
	cfgFile := dir + "/config.json"
	c := defaultConfig()
	c.DataDir = dir + "/data"
	c.PasswordHash = hashPassword("secret-pass")
	if mut != nil {
		mut(&c)
	}
	if err := saveConfigFile(cfgFile, c); err != nil {
		t.Fatal(err)
	}
	cs, _ := NewConfigStore(cfgFile)
	s, err := NewServer(cs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func proxyGet(h http.Handler, u string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", u, nil) // absolute URI = forward-proxy request
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAptProxyCache(t *testing.T) {
	var hits atomic.Int32
	files := map[string]string{
		"/ubuntu/pool/main/s/socat/socat_1.0_amd64.deb":                             "DEB-SOCAT",
		"/ubuntu/dists/noble/InRelease":                                             "RELEASE-1",
		"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + sha256Hex("PKGS"): "PKGS",
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if b, ok := files[r.URL.Path]; ok {
			w.Write([]byte(b))
			return
		}
		http.NotFound(w, r)
	}))
	upURL := up.URL
	s := testServer(t, func(c *Config) { c.AptUpstream = map[string]string{"ubuntu": upURL + "/ubuntu"} })
	h := s.Handler()
	deb := upURL + "/ubuntu/pool/main/s/socat/socat_1.0_amd64.deb"

	// miss -> upstream, tee into cache
	w := proxyGet(h, deb, nil)
	if w.Code != 200 || w.Body.String() != "DEB-SOCAT" || w.Header().Get("X-Sahand-Mirror") != "upstream" {
		t.Fatalf("miss: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	n := hits.Load()
	// hit -> cache, no upstream request
	w = proxyGet(h, deb, nil)
	if w.Code != 200 || w.Body.String() != "DEB-SOCAT" || w.Header().Get("X-Sahand-Mirror") != "cache" || hits.Load() != n {
		t.Fatalf("hit: %d %q %v hits=%d", w.Code, w.Body.String(), w.Header(), hits.Load())
	}
	// index file + by-hash
	if w = proxyGet(h, upURL+"/ubuntu/dists/noble/InRelease", nil); w.Body.String() != "RELEASE-1" {
		t.Fatalf("InRelease: %d %s", w.Code, w.Body.String())
	}
	if w = proxyGet(h, upURL+"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/"+sha256Hex("PKGS"), nil); w.Body.String() != "PKGS" {
		t.Fatalf("by-hash: %d", w.Code)
	}
	// upstream 404 is final
	if w = proxyGet(h, upURL+"/ubuntu/pool/main/n/nope/nope_1_amd64.deb", nil); w.Code != 404 {
		t.Fatalf("404: %d", w.Code)
	}
	// not an apt path / CONNECT
	if w = proxyGet(h, upURL+"/index.html", nil); w.Code != 403 {
		t.Fatalf("non-apt path: %d", w.Code)
	}
	cr := httptest.NewRequest("CONNECT", "http://example.com:443", nil)
	cr.RequestURI = "example.com:443"
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, cr)
	if cw.Code != 405 {
		t.Fatalf("CONNECT: %d", cw.Code)
	}

	// ---- upstream goes away (shutdown) ----
	up.Close()
	// make the cached index stale so the proxy tries upstream first
	cp, _ := s.aptCachePath("ubuntu/dists/noble/InRelease")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(cp, old, old)
	if w = proxyGet(h, upURL+"/ubuntu/dists/noble/InRelease", nil); w.Code != 200 || w.Body.String() != "RELEASE-1" || w.Header().Get("X-Sahand-Mirror") != "stale-cache" {
		t.Fatalf("offline stale index: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w = proxyGet(h, deb, nil); w.Code != 200 || w.Body.String() != "DEB-SOCAT" {
		t.Fatalf("offline deb: %d", w.Code)
	}
	// the same file requested from another mirror host of the family hits the shared cache
	if w = proxyGet(h, "http://de.archive.ubuntu.com/ubuntu/pool/main/s/socat/socat_1.0_amd64.deb", nil); w.Code != 200 || w.Body.String() != "DEB-SOCAT" {
		t.Fatalf("family-shared cache: %d %s", w.Code, w.Body.String())
	}
	// /apt/<host>/<path> form
	if w = get(t, h, "/apt/archive.ubuntu.com/ubuntu/pool/main/s/socat/socat_1.0_amd64.deb"); w.Code != 200 || w.Body.String() != "DEB-SOCAT" {
		t.Fatalf("/apt path: %d", w.Code)
	}
	// uncached while offline -> 502
	if w = proxyGet(h, upURL+"/ubuntu/pool/main/x/x/x_1_amd64.deb", nil); w.Code != 502 {
		t.Fatalf("offline miss: %d", w.Code)
	}
}

func TestAptProxyAccessKey(t *testing.T) {
	s := testServer(t, func(c *Config) { c.AccessKey = "key123456" })
	h := s.Handler()
	u := "http://127.0.0.1:1/ubuntu/pool/main/a/a/a_1_amd64.deb"
	if w := proxyGet(h, u, nil); w.Code != 407 {
		t.Fatalf("no key: %d", w.Code)
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("sm:key123456"))
	if w := proxyGet(h, u, map[string]string{"Proxy-Authorization": auth}); w.Code == 407 {
		t.Fatalf("with key still 407")
	}
	if got := aptProxyURL("http://10.0.0.5:8686/", "key123456"); got != "http://sm:key123456@10.0.0.5:8686/" {
		t.Errorf("aptProxyURL: %s", got)
	}
	if got := aptProxyURL("https://10.0.0.5:8686", ""); got != "" {
		t.Errorf("https hub: %s", got)
	}
}

func TestAptKey(t *testing.T) {
	s := testServer(t, func(c *Config) { c.AptInternal = map[string]string{"ubuntu": "http://mirror.example.ir/ubuntu"} })
	cases := map[string]string{
		"http://de.archive.ubuntu.com/ubuntu/pool/main/s/socat/x.deb":      "ubuntu/pool/main/s/socat/x.deb",
		"http://archive.ubuntu.com/ubuntu/dists/noble/InRelease":           "ubuntu/dists/noble/InRelease",
		"http://security.ubuntu.com/ubuntu/dists/noble-security/InRelease": "ubuntu/dists/noble-security/InRelease",
		"http://ports.ubuntu.com/ubuntu-ports/dists/noble/InRelease":       "ubuntu-ports/dists/noble/InRelease",
		"http://deb.debian.org/debian/dists/bookworm/InRelease":            "debian/dists/bookworm/InRelease",
		"http://deb.debian.org/debian-security/dists/bookworm-security/x":  "debian-security/dists/bookworm-security/x",
		"http://mirror.example.ir/ubuntu/pool/a.deb":                       "ubuntu/pool/a.deb",
		"http://download.docker.com/linux/ubuntu/dists/noble/InRelease":    "_hosts/download.docker.com/linux/ubuntu/dists/noble/InRelease",
		"http://evil.example/ubuntu/dists/noble/InRelease":                 "_hosts/evil.example/ubuntu/dists/noble/InRelease",
	}
	for in, want := range cases {
		u, _ := url.Parse(in)
		if got := s.aptKey(u); got != want {
			t.Errorf("%s -> %s want %s", in, got, want)
		}
	}
	u, _ := url.Parse("http://de.archive.ubuntu.com/ubuntu/pool/a.deb")
	c := s.aptCandidates(u, s.aptKey(u))
	if len(c) != 3 || c[1] != "http://archive.ubuntu.com/ubuntu/pool/a.deb" || c[2] != "http://mirror.example.ir/ubuntu/pool/a.deb" {
		t.Errorf("candidates: %v", c)
	}
}

func gz(t *testing.T, s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

// TestPrefetch runs the whole prefetch pipeline against a fake archive, then
// takes the archive offline and installs through the proxy from cache.
func TestPrefetch(t *testing.T) {
	debs := map[string]string{}
	pk := packagesFixture
	for _, n := range []string{"SOCAT", "LIBC6", "LIBGCC", "LIBSSL", "LIBWRAP", "POSTFIX"} {
		body := "DEB" + n[:1]
		sum := sha256.Sum256([]byte(body))
		pk = strings.Replace(pk, "SUM_"+n+"\n", hex.EncodeToString(sum[:])+"\n", 1)
		debs[n] = body
	}
	files := map[string][]byte{}
	// simpler: map file -> body using the order in the fixture
	order := []string{"SOCAT", "LIBC6", "LIBGCC", "LIBSSL", "LIBWRAP", "POSTFIX"}
	i := 0
	for _, line := range strings.Split(pk, "\n") {
		if strings.HasPrefix(line, "Filename: ") {
			files["/ubuntu/"+strings.TrimPrefix(line, "Filename: ")] = []byte(debs[order[i]])
			i++
		}
	}
	pgz := gz(t, pk)
	empty := gz(t, "")
	rel := func(p []byte) string {
		return fmt.Sprintf("Origin: Ubuntu\nAcquire-By-Hash: yes\nSHA256:\n %s %d main/binary-amd64/Packages.gz\n", sha256Hex(string(p)), len(p))
	}
	files["/ubuntu/dists/noble/InRelease"] = []byte(rel(pgz))
	files["/ubuntu/dists/noble-updates/InRelease"] = []byte(rel(empty))
	files["/ubuntu/dists/noble-security/InRelease"] = []byte(rel(empty))
	files["/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/"+sha256Hex(string(pgz))] = pgz
	files["/ubuntu/dists/noble-updates/main/binary-amd64/by-hash/SHA256/"+sha256Hex(string(empty))] = empty
	files["/ubuntu/dists/noble-security/main/binary-amd64/by-hash/SHA256/"+sha256Hex(string(empty))] = empty
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	s := testServer(t, func(c *Config) { c.AptUpstream = map[string]string{"ubuntu": up.URL + "/ubuntu"} })
	err := s.prefetch(context.Background(), []string{"socat", "no-such-pkg"}, []AptTarget{{"ubuntu", "noble", "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	st := s.store.Snapshot()
	ts := st.Apt.Targets["ubuntu/noble/amd64"]
	if ts == nil || ts.Packages["socat"] == "" || ts.Files != 5 || len(ts.Missing) != 1 {
		t.Fatalf("target state: %+v", ts)
	}
	up.Close() // shutdown
	h := s.Handler()
	for _, p := range []string{"/ubuntu/dists/noble/InRelease", "/ubuntu/dists/noble/main/binary-amd64/Packages.gz",
		"/ubuntu/pool/main/s/socat/socat_1.8.0.0-4build3_amd64.deb", "/ubuntu/pool/main/t/tcp-wrappers/libwrap0_7.6.q-33_amd64.deb"} {
		w := proxyGet(h, "http://de.archive.ubuntu.com"+p, nil)
		if w.Code != 200 {
			t.Errorf("offline %s: %d %s", p, w.Code, w.Body.String())
		}
	}
	if w := proxyGet(h, "http://de.archive.ubuntu.com/ubuntu/pool/main/p/postfix/postfix_3.8.6-1build2_amd64.deb", nil); w.Code == 200 {
		t.Error("postfix was not requested but is cached")
	}
	// readiness now says socat is handled for this target
	a := &App{ID: "x", Archs: []string{"amd64"}, Readiness: &Readiness{Findings: []Finding{{Kind: "pkg", PM: "apt", Name: "socat"}}}}
	cfg := s.cfg.Get()
	cfg.AptTargets = []string{"ubuntu/noble"}
	items, _ := evaluateReadiness(a, st, cfg)
	if items[0].Status != "handled" {
		t.Errorf("readiness: %+v", items[0])
	}
}

func TestMyIP(t *testing.T) {
	s := testServer(t, nil)
	h := s.Handler()
	r := httptest.NewRequest("GET", "/myip", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Body.String() != "10.1.2.3\n" {
		t.Errorf("myip: %q", w.Body.String())
	}
	s.cfg.Update(func(c *Config) { c.TrustXFF = true })
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Body.String() != "1.2.3.4\n" {
		t.Errorf("myip xff: %q", w.Body.String())
	}
	r = httptest.NewRequest("GET", "/myip?format=json", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"ip":"10.1.2.3"`) {
		t.Errorf("myip json: %q", w.Body.String())
	}
}
