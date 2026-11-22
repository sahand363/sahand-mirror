package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var errBusy = errors.New("a sync is already running")

type Syncer struct {
	store *Store
	cfg   func() Config
	data  string
	mu    sync.Mutex
	newGH func(token string) *GH
}

func NewSyncer(store *Store, cfg func() Config, data string) *Syncer {
	return &Syncer{store: store, cfg: cfg, data: data, newGH: newGH}
}

// Start runs a sync in the background; ids == nil means every app.
func (s *Syncer) Start(ids []string) error {
	if !s.mu.TryLock() {
		return errBusy
	}
	go func() {
		defer s.mu.Unlock()
		if err := s.run(context.Background(), ids); err != nil {
			log.Printf("sync: %v", err)
		}
	}()
	return nil
}

// Run syncs synchronously (CLI / collect).
func (s *Syncer) Run(ctx context.Context, ids []string) error {
	if !s.mu.TryLock() {
		return errBusy
	}
	defer s.mu.Unlock()
	return s.run(ctx, ids)
}

func (s *Syncer) progress(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	s.store.Mutate(func(st *State) { st.Sync.Progress = msg })
}

func (s *Syncer) run(ctx context.Context, ids []string) error {
	start := time.Now()
	s.store.Update(func(st *State) { st.Sync.Running, st.Sync.LastAttempt, st.Sync.Progress = true, start, "" })
	if ids == nil {
		s.store.View(func(st *State) {
			for _, a := range st.Apps {
				ids = append(ids, a.ID)
			}
		})
	}
	gh := s.newGH(s.cfg().GitHubToken)
	run := &syncRun{gh: gh, done: map[string]error{}}
	var errs []string
	for _, id := range ids {
		if err := s.syncApp(ctx, run, id); err != nil {
			errs = append(errs, id+": "+err.Error())
			log.Printf("sync %s: %v", id, err)
		}
	}
	s.store.Update(func(st *State) {
		st.Sync.Running, st.Sync.Progress = false, ""
		if run.ok > 0 {
			st.Sync.LastOK = time.Now()
		}
		st.Sync.LastError = strings.Join(errs, "\n")
		s.pruneLocked(st)
	})
	if len(errs) > 0 {
		return fmt.Errorf("%d app(s) failed: %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

type syncRun struct {
	gh    *GH
	done  map[string]error // repo key -> result in this run
	infos map[string]repoInfo
	ok    int // successful GitHub fetches (proves connectivity)
}

func (r *syncRun) info(ctx context.Context, owner, repo string) (repoInfo, error) {
	if r.infos == nil {
		r.infos = map[string]repoInfo{}
	}
	k := repoKey(owner, repo)
	if i, ok := r.infos[k]; ok {
		return i, nil
	}
	i, err := r.gh.repoInfo(ctx, owner, repo)
	if err == nil {
		r.infos[k] = i
		r.ok++
	}
	return i, err
}

func (s *Syncer) syncApp(ctx context.Context, run *syncRun, id string) error {
	var a App
	found := false
	s.store.View(func(st *State) {
		if p := st.App(id); p != nil {
			a, found = *p, true
		}
	})
	if !found {
		return fmt.Errorf("no such app")
	}
	s.progress("%s: تحلیل اسکریپت نصب", a.ID)
	gh := run.gh
	var notes []string
	if a.Kind == "repo" && a.Command == "" && a.Repo != "" {
		if cmd := s.detectCommand(ctx, run, a.Repo); cmd != "" {
			a.Command = cmd
			notes = append(notes, "دستور نصب از README/install.sh پیدا شد")
		} else {
			notes = append(notes, "دستور نصب پیدا نشد؛ فقط ریلیزها آینه می‌شوند")
		}
	}

	repos := []string{}
	seenRepo := map[string]bool{}
	addRepo := func(name string) {
		if k := strings.ToLower(name); !seenRepo[k] {
			seenRepo[k] = true
			repos = append(repos, name)
		}
	}
	if a.Repo != "" {
		addRepo(a.Repo)
	}
	var raws []RawRef
	var exts []string
	seenExt := map[string]bool{}
	var discErr error
	scan := func(res ScanResult) []string {
		var next []string
		for _, r := range res.Repos {
			addRepo(r)
		}
		for _, r := range res.Raw {
			raws = append(raws, r)
			if strings.HasSuffix(r.Path, ".sh") {
				next = append(next, r.URL())
			}
		}
		for _, e := range res.Ext {
			if !seenExt[e] {
				seenExt[e] = true
				exts = append(exts, e)
			}
			mirrored := false
			s.store.View(func(st *State) { mirrored = st.Ext[e] != nil && st.Ext[e].Mirror })
			if mirrored && literal(e) {
				next = append(next, e)
			}
		}
		return next
	}
	if a.Command != "" {
		queue := []string{}
		if u := firstURL(a.Command); u != "" {
			queue = append(queue, u)
			if c := classifyURL(u); c.Kind == "ext" {
				// the install script itself lives off GitHub: always mirror it
				s.store.Update(func(st *State) {
					st.Ext[u] = orNewExt(st.Ext[u], u)
					st.Ext[u].Mirror = true
				})
				seenExt[u], exts = true, append(exts, u)
			} else if c.Kind == "raw" || c.Kind == "github" {
				addRepo(c.Owner + "/" + c.Repo)
			}
		}
		visited := map[string]bool{}
		for fetched := 0; len(queue) > 0 && fetched < 25; {
			u := queue[0]
			queue = queue[1:]
			fetchURL := u
			if c := classifyURL(u); c.Kind == "raw" && !literal(c.Raw.Ref) {
				if inf, err := run.info(ctx, c.Owner, c.Repo); err == nil {
					c.Raw.Ref = inf.DefaultBranch
					fetchURL = c.Raw.URL()
				}
			}
			if visited[fetchURL] || !literal(fetchURL) {
				continue
			}
			visited[fetchURL] = true
			fetched++
			body, err := gh.getBytes(ctx, fetchURL, 4<<20)
			if err != nil {
				if fetched == 1 {
					discErr = fmt.Errorf("install script: %w", err)
				}
				continue
			}
			run.ok++
			if looksLikeScript(body) {
				queue = append(queue, scan(scanScript(string(body)))...)
			}
		}
	}
	if a.Recipe != "" {
		scan(scanScript(a.Recipe))
	}

	// persist discovery
	var repoKeys []string
	for _, r := range repos {
		repoKeys = append(repoKeys, strings.ToLower(r))
	}
	var rawURLs []string
	seenRaw := map[string]bool{}
	for _, r := range raws {
		if k := strings.ToLower(r.URL()); literal(r.Ref) && literal(r.Path) && !seenRaw[k] {
			seenRaw[k] = true
			rawURLs = append(rawURLs, r.URL())
		}
	}
	s.store.Update(func(st *State) {
		p := st.App(a.ID)
		if p == nil {
			return
		}
		p.Command, p.Repos, p.Raw, p.Externals = a.Command, repoKeys, rawURLs, exts
		for _, e := range exts {
			st.Ext[e] = orNewExt(st.Ext[e], e)
		}
	})

	var errs []string
	if discErr != nil {
		errs = append(errs, discErr.Error())
	}
	// external static files
	for _, e := range exts {
		var x Ext
		s.store.View(func(st *State) { x = *st.Ext[e] })
		if !x.Mirror || x.Dynamic {
			continue
		}
		s.progress("%s: دریافت %s", a.ID, e)
		n, _, err := gh.download(ctx, e, extPath(s.data, e))
		s.store.Update(func(st *State) {
			p := st.Ext[e]
			if err != nil {
				p.Error = err.Error()
				return
			}
			p.Size, p.Fetched, p.Error = n, time.Now(), ""
		})
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			run.ok++
		}
	}
	// repositories
	for _, r := range repos {
		k := strings.ToLower(r)
		err, done := run.done[k]
		if !done {
			err = s.syncRepo(ctx, run, r)
			run.done[k] = err
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	// literal raw files (refs that are neither default branch nor mirrored tags)
	for _, u := range rawURLs {
		r := classifyURL(u).Raw
		k := repoKey(r.Owner, r.Repo)
		dst, err := safeJoin(repoDir(s.data, k), "raw", tagDir(r.Ref), r.Path)
		if err != nil {
			continue
		}
		if _, _, err := gh.download(ctx, r.URL(), dst); err == nil {
			s.store.Update(func(st *State) {
				if rp := st.Repos[k]; rp != nil && !contains(rp.Raw, r.Ref+"/"+r.Path) {
					rp.Raw = append(rp.Raw, r.Ref+"/"+r.Path)
				}
			})
		}
	}
	s.store.Update(func(st *State) {
		p := st.App(a.ID)
		if p == nil {
			return
		}
		p.LastSync, p.Note, p.Error = time.Now(), strings.Join(notes, "؛ "), strings.Join(errs, "\n")
	})
	if len(errs) > 0 {
		return errors.New(errs[0])
	}
	return nil
}

func orNewExt(e *Ext, u string) *Ext {
	if e != nil {
		return e
	}
	return &Ext{URL: u, Dynamic: !literal(u)}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (s *Syncer) detectCommand(ctx context.Context, run *syncRun, full string) string {
	o, r := splitKey(full)
	inf, err := run.info(ctx, o, r)
	if err != nil {
		return ""
	}
	base := run.gh.raw + "/" + inf.Name + "/" + inf.DefaultBranch + "/"
	for _, f := range []string{"README.md", "readme.md", "README.MD", "README"} {
		if b, err := run.gh.getBytes(ctx, base+f, 2<<20); err == nil {
			if cmd := findInstallCommand(string(b), o, r); cmd != "" {
				return cmd
			}
			break
		}
	}
	for _, f := range []string{"install.sh", "scripts/install.sh"} {
		if resp, err := run.gh.do(ctx, "HEAD", base+f); err == nil {
			resp.Body.Close()
			return "bash <(curl -Ls " + base + f + ")"
		}
	}
	return ""
}

type repoPolicy struct {
	keep    int
	pre     bool
	archs   map[string]bool // nil = all
	exclude *regexp.Regexp
}

func policyFor(st *State, key string, globalKeep int) repoPolicy {
	p := repoPolicy{keep: 0}
	all := false
	for _, a := range st.Apps {
		if !contains(a.Repos, key) {
			continue
		}
		k := a.Keep
		if k <= 0 {
			k = globalKeep
		}
		p.keep = max(p.keep, k)
		p.pre = p.pre || a.Pre
		if len(a.Archs) == 0 {
			all = true
		} else if !all {
			if p.archs == nil {
				p.archs = map[string]bool{}
			}
			for _, x := range a.Archs {
				p.archs[x] = true
			}
		}
		if a.Exclude != "" && strings.EqualFold(a.Repo, key) && p.exclude == nil {
			p.exclude, _ = regexp.Compile(a.Exclude)
		}
	}
	if all {
		p.archs = nil
	}
	if p.keep <= 0 {
		p.keep = max(globalKeep, 1)
	}
	return p
}

var (
	otherOSRe    = regexp.MustCompile(`(?i)(windows|win32|win64|darwin|macos|osx|freebsd|openbsd|netbsd|\.exe$|\.msi$|\.dmg$|\.pkg$)`)
	archPatterns = []struct {
		arch string
		re   *regexp.Regexp
	}{
		{"other", regexp.MustCompile(`(?i)(armv?[56]([^0-9]|$)|s390x|mips|riscv|ppc64|loong64|loongarch)`)},
		{"arm64", regexp.MustCompile(`(?i)(arm64|aarch64|armv8)`)},
		{"armv7", regexp.MustCompile(`(?i)(armv7|arm32|armhf|armeabi|armel|(^|[-_.])arm([-_.]|$))`)},
		{"amd64", regexp.MustCompile(`(?i)(amd64|x86[_-]64|x64|64bit)`)},
		{"386", regexp.MustCompile(`(?i)(386|i686|x86|32bit)`)},
	}
)

// assetArch classifies an asset name: "otheros", an arch, or "" (neutral).
func assetArch(name string) string {
	l := sidecarRe.ReplaceAllString(strings.ToLower(name), "")
	if strings.HasSuffix(l, ".apk") || strings.HasSuffix(l, ".apk.sha256") || strings.HasSuffix(l, ".aab") {
		return "android"
	}
	if otherOSRe.MatchString(l) {
		return "otheros"
	}
	for _, p := range archPatterns {
		if p.re.MatchString(l) {
			return p.arch
		}
	}
	return ""
}

func (p repoPolicy) allows(name string) bool {
	if p.exclude != nil && p.exclude.MatchString(name) {
		return false
	}
	if p.archs == nil {
		return true
	}
	switch a := assetArch(name); a {
	case "", "android":
		return true
	default:
		return p.archs[a]
	}
}

var sidecarRe = regexp.MustCompile(`\.(sha256|sha512|sha256sum|md5|asc|sig|dgst)$`)

var famRe = regexp.MustCompile(`^[^0-9]*`)

// selectReleases keeps the newest `keep` releases plus the newest release of
// every other tag family (e.g. "android-v1.2" next to "v0.9"), so APK
// releases are not pushed out by server releases.
func selectReleases(rels []Release, keep int, pre bool) []Release {
	var cands []Release
	for _, r := range rels {
		if pre || !r.Pre {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		cands = rels
	}
	sortReleases(cands)
	fam := func(t string) string { return strings.ToLower(famRe.FindString(t)) }
	out := cands[:min(keep, len(cands))]
	have := map[string]bool{}
	for _, r := range out {
		have[fam(r.Tag)] = true
	}
	out = append([]Release(nil), out...)
	for _, r := range cands[len(out):] {
		if f := fam(r.Tag); !have[f] && len(have) < keep+4 {
			have[f] = true
			out = append(out, r)
		}
	}
	return out
}

func (s *Syncer) syncRepo(ctx context.Context, run *syncRun, name string) error {
	o, r := splitKey(name)
	key := repoKey(o, r)
	gh := run.gh
	s.progress("%s: اطلاعات مخزن", name)
	inf, err := run.info(ctx, o, r)
	if err != nil {
		s.repoErr(key, name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	o, r = splitKey(inf.Name)
	cfg := s.cfg()
	var pol repoPolicy
	var old Repo
	s.store.View(func(st *State) {
		pol = policyFor(st, key, cfg.Keep)
		if p := st.Repos[key]; p != nil {
			old = *p
		}
	})
	rels, latest, err := gh.releases(ctx, o, r)
	if err != nil {
		s.repoErr(key, inf.Name, err)
		return fmt.Errorf("%s releases: %w", inf.Name, err)
	}
	run.ok++
	kept := selectReleases(rels, pol.keep, pol.pre)
	dir := repoDir(s.data, key)
	var errs []string
	keepTags := map[string]bool{}
	for i := range kept {
		rel := &kept[i]
		keepTags[tagDir(rel.Tag)] = true
		if rel.Assets == nil {
			as, err := gh.webAssets(ctx, o, r, rel.Tag)
			if err != nil {
				errs = append(errs, err.Error())
			}
			rel.Assets = as
		}
		var prev *Release
		if old.Releases != nil {
			prev = old.release(rel.Tag)
		}
		var assets []Asset
		for _, as := range rel.Assets {
			if pol.allows(as.Name) {
				assets = append(assets, as)
			}
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		sem := make(chan struct{}, 4)
		for j := range assets {
			as := &assets[j]
			dst, err := safeJoin(dir, "releases", tagDir(rel.Tag), as.Name)
			if err != nil || !validTag(as.Name) {
				continue
			}
			if fi, err := os.Stat(dst); err == nil && (as.Size == 0 || fi.Size() == as.Size) {
				as.Size = fi.Size()
				if prev != nil {
					for _, pa := range prev.Assets {
						if pa.Name == as.Name && pa.Size == fi.Size() {
							as.SHA256 = pa.SHA256
						}
					}
				}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				s.progress("%s %s: %s", inf.Name, rel.Tag, as.Name)
				n, sum, err := gh.download(ctx, gh.assetURL(o, r, rel.Tag, as.Name), dst)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err.Error())
					as.Name = "" // drop
					return
				}
				as.Size, as.SHA256 = n, sum
			}()
		}
		wg.Wait()
		rel.Assets = rel.Assets[:0]
		for _, as := range assets {
			if as.Name != "" {
				rel.Assets = append(rel.Assets, as)
			}
		}
		// source snapshot at this tag (serves raw.githubusercontent.com/<o>/<r>/<tag>/...)
		src := filepath.Join(dir, "src", "tags", tagDir(rel.Tag)+".tar.gz")
		if fileExists(src) {
			rel.Source = true
		} else if _, _, err := gh.download(ctx, gh.tarballURL(o, r, "tags", url.PathEscape(rel.Tag)), src); err == nil {
			rel.Source = true
		} else {
			errs = append(errs, err.Error())
		}
	}
	// default branch snapshot, refreshed every sync
	s.progress("%s: سورس شاخه %s", inf.Name, inf.DefaultBranch)
	head := filepath.Join(dir, "src", "heads", tagDir(inf.DefaultBranch)+".tar.gz")
	if _, _, err := gh.download(ctx, gh.tarballURL(o, r, "heads", inf.DefaultBranch), head); err != nil {
		errs = append(errs, err.Error())
	}
	// prune releases that fell out of the window
	for _, sub := range []string{"releases", path.Join("src", "tags")} {
		ents, _ := os.ReadDir(filepath.Join(dir, sub))
		for _, e := range ents {
			n := strings.TrimSuffix(e.Name(), ".tar.gz")
			if !keepTags[n] && !strings.HasPrefix(n, ".tmp-") {
				os.RemoveAll(filepath.Join(dir, sub, e.Name()))
			}
		}
	}
	if latest == "" && len(kept) > 0 {
		for _, k := range kept {
			if !k.Pre {
				latest = k.Tag
				break
			}
		}
	}
	errText := strings.Join(errs, "\n")
	s.store.Update(func(st *State) {
		rp := &Repo{Name: inf.Name, DefaultBranch: inf.DefaultBranch, Latest: latest,
			Releases: kept, Raw: old.Raw, LastSync: time.Now(), Error: errText}
		if rp.release(latest) == nil && len(kept) > 0 {
			rp.Latest = "" // GitHub's latest was filtered out; latestRelease() falls back
		}
		st.Repos[key] = rp
	})
	if errText != "" {
		return fmt.Errorf("%s: %s", inf.Name, errs[0])
	}
	return nil
}

func (s *Syncer) repoErr(key, name string, err error) {
	s.store.Update(func(st *State) {
		if p := st.Repos[key]; p != nil {
			p.Error = err.Error()
		} else {
			st.Repos[key] = &Repo{Name: name, Error: err.Error()}
		}
	})
}

// pruneLocked removes repos no app references and unmirrored external files.
func (s *Syncer) pruneLocked(st *State) {
	used := map[string]bool{}
	usedExt := map[string]bool{}
	for _, a := range st.Apps {
		for _, k := range a.Repos {
			used[k] = true
		}
		for _, e := range a.Externals {
			usedExt[e] = true
		}
	}
	for k := range st.Repos {
		if !used[k] {
			delete(st.Repos, k)
			os.RemoveAll(repoDir(s.data, k))
		}
	}
	for u, e := range st.Ext {
		if !usedExt[u] {
			delete(st.Ext, u)
		}
		if !usedExt[u] || !e.Mirror {
			os.Remove(extPath(s.data, u))
		}
	}
}

// scheduler triggers the daily direct sync.
func (s *Syncer) scheduler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c := s.cfg()
			if !c.DirectSync || c.SyncHour < 0 || now.Hour() != c.SyncHour {
				continue
			}
			var last time.Time
			s.store.View(func(st *State) { last = st.Sync.LastAttempt })
			if y1, m1, d1 := last.Date(); y1 == now.Year() && m1 == now.Month() && d1 == now.Day() {
				continue
			}
			s.Start(nil)
		}
	}
}
