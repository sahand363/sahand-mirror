package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

type Release struct {
	Tag       string    `json:"tag"`
	Name      string    `json:"name"`
	Pre       bool      `json:"prerelease"`
	Published time.Time `json:"published"`
	Assets    []Asset   `json:"assets"`
	Source    bool      `json:"source"` // tag tarball mirrored
}

type Repo struct {
	Name          string    `json:"name"` // Owner/Repo as on GitHub
	DefaultBranch string    `json:"default_branch"`
	Latest        string    `json:"latest"`
	Releases      []Release `json:"releases"`
	Raw           []string  `json:"raw,omitempty"` // "ref/path" files stored individually
	LastSync      time.Time `json:"last_sync"`
	Error         string    `json:"error,omitempty"`
}

type Ext struct {
	URL     string    `json:"url"`
	Mirror  bool      `json:"mirror"`
	Dynamic bool      `json:"dynamic"` // contains shell variables, cannot be fetched verbatim
	Size    int64     `json:"size"`
	Fetched time.Time `json:"fetched"`
	Error   string    `json:"error,omitempty"`
}

type App struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"` // "repo" | "command"
	Repo      string     `json:"repo"` // primary owner/repo
	Command   string     `json:"command"`
	Recipe    string     `json:"recipe,omitempty"` // edited install script
	Keep      int        `json:"keep,omitempty"`   // 0 = global
	Archs     []string   `json:"archs,omitempty"`  // empty = all
	Exclude   string     `json:"exclude,omitempty"`
	Pre       bool       `json:"prerelease,omitempty"`
	Repos     []string   `json:"repos"`               // lower-case owner/repo referenced
	Raw       []string   `json:"raw,omitempty"`       // literal raw.githubusercontent URLs
	Externals []string   `json:"externals,omitempty"` // non-GitHub URLs found
	Note      string     `json:"note,omitempty"`
	Added     time.Time  `json:"added"`
	LastSync  time.Time  `json:"last_sync"`
	Error     string     `json:"error,omitempty"`
	Disabled  []string   `json:"disabled_repos,omitempty"` // repo keys excluded from mirroring
	AptExtra  []string   `json:"apt_extra,omitempty"`      // extra apt packages to prefetch
	Readiness *Readiness `json:"readiness,omitempty"`
}

// enabledRepos: referenced repos minus the ones the user switched off.
func (a *App) enabledRepos() []string {
	var out []string
	for _, k := range a.Repos {
		if !contains(a.Disabled, k) {
			out = append(out, k)
		}
	}
	return out
}

type SyncStatus struct {
	Running     bool      `json:"running"`
	Progress    string    `json:"progress"`
	LastAttempt time.Time `json:"last_attempt"`
	LastOK      time.Time `json:"last_ok"`
	LastImport  time.Time `json:"last_import"`
	LastError   string    `json:"last_error,omitempty"`
}

type State struct {
	Apps  []*App           `json:"apps"`
	Repos map[string]*Repo `json:"repos"`
	Ext   map[string]*Ext  `json:"ext"`
	Sync  SyncStatus       `json:"sync"`
	// Aliases maps a repo key as written in scripts to the canonical key
	// GitHub redirects it to (renamed/transferred repos are stored once).
	Aliases map[string]string `json:"aliases,omitempty"`
	Apt     *AptState         `json:"apt,omitempty"`
}

// canon resolves a repo key through the alias table.
func (st *State) canon(k string) string {
	if c, ok := st.Aliases[k]; ok && c != "" {
		return c
	}
	return k
}

type Store struct {
	mu   sync.RWMutex
	path string
	st   *State
}

func newState() *State {
	return &State{Repos: map[string]*Repo{}, Ext: map[string]*Ext{}}
}

func OpenStore(dataDir string, seed bool) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dataDir, "state.json"), st: newState()}
	b, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, s.st); err != nil {
			return nil, err
		}
		if s.st.Repos == nil {
			s.st.Repos = map[string]*Repo{}
		}
		if s.st.Ext == nil {
			s.st.Ext = map[string]*Ext{}
		}
		s.st.Sync.Running = false
		if s.st.Apt != nil {
			s.st.Apt.Running, s.st.Apt.Progress = false, ""
		}
	case errors.Is(err, os.ErrNotExist):
		if seed {
			for _, a := range defaultApps() {
				s.st.Apps = append(s.st.Apps, a)
			}
		}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.st, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o644)
}

// Update runs fn under the write lock and persists the state.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.st)
	return s.saveLocked()
}

// Mutate changes state without persisting (progress text).
func (s *Store) Mutate(fn func(*State)) {
	s.mu.Lock()
	fn(s.st)
	s.mu.Unlock()
}

func (s *Store) View(fn func(*State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.st)
}

// Snapshot returns a deep copy (via JSON) for lock-free reading.
func (s *Store) Snapshot() *State {
	s.mu.RLock()
	b, _ := json.Marshal(s.st)
	s.mu.RUnlock()
	st := newState()
	json.Unmarshal(b, st)
	return st
}

func (st *State) App(id string) *App {
	for _, a := range st.Apps {
		if strings.EqualFold(a.ID, id) {
			return a
		}
	}
	return nil
}

func repoKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + strings.TrimSuffix(repo, ".git"))
}

func splitKey(k string) (string, string) {
	o, r, _ := strings.Cut(k, "/")
	return o, r
}

var slugRe = regexp.MustCompile(`[^a-z0-9._-]+`)

func slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-.")
	if s == "" {
		s = "app"
	}
	return s
}

func (st *State) uniqueID(base string) string {
	id := slugify(base)
	for i := 2; st.App(id) != nil; i++ {
		id = slugify(base) + "-" + strconv.Itoa(i)
	}
	return id
}

// Latest stable mirrored release (or newest if none stable).
func (r *Repo) latestRelease() *Release {
	for i := range r.Releases {
		if r.Releases[i].Tag == r.Latest {
			return &r.Releases[i]
		}
	}
	for i := range r.Releases {
		if !r.Releases[i].Pre {
			return &r.Releases[i]
		}
	}
	if len(r.Releases) > 0 {
		return &r.Releases[0]
	}
	return nil
}

func (r *Repo) release(tag string) *Release {
	for i := range r.Releases {
		if r.Releases[i].Tag == tag {
			return &r.Releases[i]
		}
	}
	for i := range r.Releases {
		if strings.EqualFold(r.Releases[i].Tag, tag) {
			return &r.Releases[i]
		}
	}
	return nil
}

func sortReleases(rs []Release) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Published.After(rs[j].Published) })
}

// on-disk layout helpers
func repoDir(data, key string) string {
	o, r := splitKey(key)
	return filepath.Join(data, "repos", o, r)
}
func tagDir(tag string) string      { return url.PathEscape(tag) }
func extPath(data, u string) string { return filepath.Join(data, "ext", sha256Hex(u)) }

func validTag(t string) bool {
	if t == "" || t == "." || t == ".." || len(t) > 200 || strings.ContainsAny(t, "/\\") {
		return false
	}
	for _, c := range t {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func defaultApps() []*App {
	now := time.Now()
	mk := func(id, repo, cmd string) *App {
		return &App{ID: id, Name: id, Kind: "repo", Repo: repo, Command: cmd,
			Repos: []string{strings.ToLower(repo)}, Added: now}
	}
	return []*App{
		mk("zimzim", "sahand363/zimzim", "bash <(curl -Ls https://raw.githubusercontent.com/sahand363/zimzim/main/install.sh)"),
		mk("loadinme", "sahand363/loadinme", "curl -fsSL https://raw.githubusercontent.com/sahand363/loadinme/main/install.sh | bash"),
		mk("blendartai", "sahand363/blendartai", "bash <(curl -fsSL https://raw.githubusercontent.com/sahand363/blendartai/main/scripts/install.sh)"),
		mk("sahand-mirror", "sahand363/sahand-mirror", "bash <(curl -Ls https://raw.githubusercontent.com/sahand363/sahand-mirror/main/install.sh)"),
	}
}
