package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	cfg    *ConfigStore
	store  *Store
	sync   *Syncer
	emu    *Emulator
	ca     *CA
	data   string
	mu     sync.Mutex
	sess   map[string]time.Time
	fails  map[string]*loginFail
	diskMu sync.Mutex
	diskAt time.Time
	diskN  int64
	busy   sync.Mutex // import/export exclusivity
}

type loginFail struct {
	n     int
	until time.Time
}

func NewServer(cfg *ConfigStore) (*Server, error) {
	c := cfg.Get()
	store, err := OpenStore(c.DataDir, true)
	if err != nil {
		return nil, err
	}
	ca, err := LoadOrCreateCA(c.DataDir)
	if err != nil {
		return nil, err
	}
	os.MkdirAll(filepath.Join(c.DataDir, "tmp"), 0o755)
	s := &Server{cfg: cfg, store: store, ca: ca, data: c.DataDir,
		sess: map[string]time.Time{}, fails: map[string]*loginFail{}}
	s.sync = NewSyncer(store, cfg.Get, c.DataDir)
	s.emu = &Emulator{store: store, data: c.DataDir}
	return s, nil
}

func (s *Server) hubURL(r *http.Request) string {
	if u := strings.TrimRight(s.cfg.Get().PublicURL, "/"); u != "" {
		return u
	}
	return baseURL(r)
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// ---------- routing ----------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/gh/", s.public(s.emu.ServeHTTP))
	mux.HandleFunc("/ext", s.public(s.emu.ServeExt))
	mux.HandleFunc("/i/", s.public(s.handleInstall))
	mux.HandleFunc("/d", s.public(s.handleDownloads))
	mux.HandleFunc("/ca.crt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="sahand-mirror-ca.crt"`)
		w.Write(s.ca.CertPEM)
	})
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/", s.auth(s.handleAPI))
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'")
		w.Header().Set("X-Frame-Options", "DENY")
		static.ServeHTTP(w, r)
	})
	return logRequests(mux)
}

type statusWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// logRequests logs public mirror hits (what target servers fetch).
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		p := r.URL.Path
		if strings.HasPrefix(p, "/gh/") || strings.HasPrefix(p, "/i/") || p == "/ext" || p == "/d" {
			log.Printf("hit %s %s %s %d %d", clientIP(r), r.Method, r.URL.RequestURI(), sw.code, sw.n)
		}
	})
}

// public guards mirror endpoints with the optional access key.
func (s *Server) public(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.cfg.Get().AccessKey
		if key != "" {
			got := r.URL.Query().Get("k")
			if got == "" {
				got = r.Header.Get("X-Mirror-Key")
			}
			if got == "" {
				if c, err := r.Cookie("sm_k"); err == nil {
					got = c.Value
				}
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 && !s.loggedIn(r) {
				http.Error(w, "access key required (?k=)", http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("k") != "" {
				http.SetCookie(w, &http.Cookie{Name: "sm_k", Value: got, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
			}
		}
		h(w, r)
	}
}

// ---------- auth ----------

func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie("sm_session")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sess[c.Value]
	if !ok || time.Now().After(exp) {
		delete(s.sess, c.Value)
		return false
	}
	return true
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			writeJSON(w, 401, map[string]string{"error": "login required"})
			return
		}
		if r.Method != "GET" && r.Header.Get("X-Requested-With") != "sahand-mirror" {
			writeJSON(w, 403, map[string]string{"error": "missing CSRF header"})
			return
		}
		h(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	ip := clientIP(r)
	s.mu.Lock()
	f := s.fails[ip]
	if f != nil && time.Now().Before(f.until) {
		wait := time.Until(f.until).Round(time.Second)
		s.mu.Unlock()
		writeJSON(w, 429, map[string]string{"error": fmt.Sprintf("تلاش زیاد؛ %v دیگر دوباره امتحان کنید", wait)})
		return
	}
	s.mu.Unlock()
	var in struct{ Username, Password string }
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	c := s.cfg.Get()
	userOK := subtle.ConstantTimeCompare([]byte(in.Username), []byte(c.AdminUser)) == 1
	if !checkPassword(c.PasswordHash, in.Password) || !userOK {
		s.mu.Lock()
		if f == nil {
			f = &loginFail{}
			s.fails[ip] = f
		}
		f.n++
		if f.n >= 5 {
			f.until, f.n = time.Now().Add(5*time.Minute), 0
		}
		s.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
		writeJSON(w, 401, map[string]string{"error": "نام کاربری یا رمز اشتباه است"})
		return
	}
	tok := randHex(32)
	s.mu.Lock()
	delete(s.fails, ip)
	s.sess[tok] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "sm_session", Value: tok, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sm_session"); err == nil {
		s.mu.Lock()
		delete(s.sess, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "sm_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- admin API ----------

var ownerRepoRe = regexp.MustCompile(`^(?:https?://github\.com/)?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/")
	seg := strings.Split(strings.Trim(p, "/"), "/")
	bad := func(err string) { writeJSON(w, 400, map[string]string{"error": err}) }
	switch {
	case p == "status":
		writeJSON(w, 200, s.status(r))
	case p == "apps" && r.Method == "GET":
		st := s.store.Snapshot()
		writeJSON(w, 200, map[string]any{"apps": st.Apps, "repos": st.Repos})
	case p == "apps" && r.Method == "POST":
		var in struct {
			Repo, Command, Name string
			Archs               []string
			Keep                int
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			bad("bad json")
			return
		}
		a, err := s.addApp(in.Repo, in.Command, in.Name, in.Archs, in.Keep)
		if err != nil {
			bad(err.Error())
			return
		}
		s.sync.Start([]string{a.ID})
		writeJSON(w, 200, a)
	case p == "apps.json":
		w.Header().Set("Content-Disposition", `attachment; filename="apps.json"`)
		writeJSON(w, 200, s.store.Snapshot().Apps)
	case p == "sync" && r.Method == "POST":
		if err := s.sync.Start(nil); err != nil {
			bad(err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(seg) >= 2 && seg[0] == "apps":
		s.handleApp(w, r, seg[1], seg[2:])
	case p == "ext" && r.Method == "POST":
		var in struct {
			URL    string
			Mirror bool
		}
		json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in)
		found := false
		s.store.Update(func(st *State) {
			if e := st.Ext[in.URL]; e != nil && !e.Dynamic {
				e.Mirror, found = in.Mirror, true
			}
		})
		if !found {
			bad("unknown or dynamic URL")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case p == "settings" && r.Method == "GET":
		c := s.cfg.Get()
		writeJSON(w, 200, map[string]any{"public_url": c.PublicURL, "listen": c.Listen, "direct_sync": c.DirectSync,
			"sync_hour": c.SyncHour, "keep": c.Keep, "token_set": c.GitHubToken != "", "access_key": c.AccessKey,
			"tls": c.TLS, "apt_mirror_hint": c.AptMirrorHint, "admin_user": c.AdminUser, "max_upload_mb": c.MaxUploadMB})
	case p == "settings" && r.Method == "PUT":
		var in struct {
			PublicURL     *string `json:"public_url"`
			DirectSync    *bool   `json:"direct_sync"`
			SyncHour      *int    `json:"sync_hour"`
			Keep          *int    `json:"keep"`
			GitHubToken   *string `json:"github_token"`
			AccessKey     *string `json:"access_key"`
			TLS           *bool   `json:"tls"`
			AptMirrorHint *string `json:"apt_mirror_hint"`
			MaxUploadMB   *int64  `json:"max_upload_mb"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			bad("bad json")
			return
		}
		if in.PublicURL != nil && *in.PublicURL != "" {
			if u, err := url.Parse(*in.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				bad("آدرس عمومی باید مثل http://10.0.0.5:8686 باشد")
				return
			}
		}
		if in.AccessKey != nil && *in.AccessKey != "" && !regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`).MatchString(*in.AccessKey) {
			bad("کلید دسترسی: ۶ تا ۶۴ حرف/عدد")
			return
		}
		err := s.cfg.Update(func(c *Config) {
			set := func(dst *string, v *string) {
				if v != nil {
					*dst = strings.TrimSpace(*v)
				}
			}
			set(&c.PublicURL, in.PublicURL)
			set(&c.AccessKey, in.AccessKey)
			set(&c.AptMirrorHint, in.AptMirrorHint)
			if in.PublicURL != nil {
				c.PublicURL = strings.TrimRight(c.PublicURL, "/")
			}
			if in.GitHubToken != nil && *in.GitHubToken != "" { // write-only; "-" clears
				c.GitHubToken = strings.TrimSpace(*in.GitHubToken)
				if c.GitHubToken == "-" {
					c.GitHubToken = ""
				}
			}
			if in.DirectSync != nil {
				c.DirectSync = *in.DirectSync
			}
			if in.SyncHour != nil && *in.SyncHour >= -1 && *in.SyncHour <= 23 {
				c.SyncHour = *in.SyncHour
			}
			if in.Keep != nil && *in.Keep >= 1 && *in.Keep <= 50 {
				c.Keep = *in.Keep
			}
			if in.TLS != nil {
				c.TLS = *in.TLS
			}
			if in.MaxUploadMB != nil && *in.MaxUploadMB > 0 {
				c.MaxUploadMB = *in.MaxUploadMB
			}
		})
		if err != nil {
			bad(err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case p == "password" && r.Method == "POST":
		var in struct{ Old, New string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		if !checkPassword(s.cfg.Get().PasswordHash, in.Old) {
			bad("رمز فعلی اشتباه است")
			return
		}
		if len(in.New) < 8 {
			bad("رمز جدید حداقل ۸ کاراکتر")
			return
		}
		s.cfg.Update(func(c *Config) { c.PasswordHash = hashPassword(in.New) })
		writeJSON(w, 200, map[string]bool{"ok": true})
	case p == "certs" && r.Method == "POST":
		var in struct {
			Hosts string
			Days  int
		}
		json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in)
		z, err := s.ca.IssueZip(strings.FieldsFunc(in.Hosts, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' }), in.Days)
		if err != nil {
			bad(err.Error())
			return
		}
		name := regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(strings.Fields(strings.ReplaceAll(in.Hosts, ",", " "))[0], "_")
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="cert-`+name+`.zip"`)
		w.Write(z)
	case p == "bundle/export" && r.Method == "GET":
		if !s.busy.TryLock() {
			bad("عملیات دیگری در جریان است")
			return
		}
		defer s.busy.Unlock()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="sahand-mirror-bundle-`+time.Now().Format("20060102-1504")+`.tar.gz"`)
		if _, err := ExportBundle(w, s.data, s.store.Snapshot()); err != nil {
			log.Printf("export: %v", err)
		}
	case p == "bundle/import" && r.Method == "POST":
		s.handleUpload(w, r)
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.busy.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "عملیات دیگری در جریان است"})
		return
	}
	defer s.busy.Unlock()
	limit := s.cfg.Get().MaxUploadMB << 20
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	tmp, err := os.CreateTemp(filepath.Join(s.data, "tmp"), "upload-*.tar.gz")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, r.Body)
	tmp.Close()
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "آپلود ناموفق یا بزرگ‌تر از سقف مجاز: " + err.Error()})
		return
	}
	m, err := ImportBundle(tmp.Name(), s.data, s.store)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "files": len(m.Files), "apps": m.Apps, "created": m.Created})
}

func (s *Server) addApp(repo, command, name string, archs []string, keep int) (*App, error) {
	a := &App{Added: time.Now(), Keep: max(0, min(keep, 50))}
	for _, x := range archs {
		if contains(validArchs, x) {
			a.Archs = append(a.Archs, x)
		}
	}
	repo, command = strings.TrimSpace(repo), strings.TrimSpace(command)
	switch {
	case repo != "":
		m := ownerRepoRe.FindStringSubmatch(repo)
		if m == nil {
			return nil, errors.New("مخزن را به شکل owner/repo وارد کنید")
		}
		a.Kind, a.Repo = "repo", m[1]+"/"+m[2]
		a.Repos = []string{repoKey(m[1], m[2])}
		if name == "" {
			name = m[2]
		}
	case command != "":
		if len(command) > 4000 || strings.ContainsAny(command, "\n\r") {
			return nil, errors.New("دستور نصب باید یک خط باشد")
		}
		u := firstURL(command)
		if u == "" {
			return nil, errors.New("در دستور نصب آدرسی پیدا نشد")
		}
		a.Kind, a.Command = "command", command
		if c := classifyURL(u); c.Kind == "raw" || c.Kind == "github" {
			a.Repo = c.Owner + "/" + c.Repo
			a.Repos = []string{repoKey(c.Owner, c.Repo)}
			if name == "" {
				name = c.Repo
			}
		} else if name == "" {
			pu, _ := url.Parse(u)
			name = pu.Hostname()
		}
	default:
		return nil, errors.New("مخزن یا دستور نصب لازم است")
	}
	var err error
	s.store.Update(func(st *State) {
		for _, x := range st.Apps {
			if (a.Repo != "" && strings.EqualFold(x.Repo, a.Repo) && x.Command == a.Command) || (a.Command != "" && x.Command == a.Command) {
				err = fmt.Errorf("این برنامه قبلاً با شناسه %s اضافه شده", x.ID)
				return
			}
		}
		a.ID = st.uniqueID(name)
		a.Name = name
		st.Apps = append(st.Apps, a)
	})
	return a, err
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request, id string, rest []string) {
	var a *App
	s.store.View(func(st *State) {
		if p := st.App(id); p != nil {
			c := *p
			a = &c
		}
	})
	if a == nil {
		writeJSON(w, 404, map[string]string{"error": "no such app"})
		return
	}
	switch {
	case len(rest) == 0 && r.Method == "GET":
		st := s.store.Snapshot()
		repos := map[string]*Repo{}
		for _, k := range a.Repos {
			if rp := st.Repos[k]; rp != nil {
				repos[k] = rp
			}
		}
		ext := []*Ext{}
		for _, u := range a.Externals {
			if e := st.Ext[u]; e != nil {
				ext = append(ext, e)
			}
		}
		c := s.cfg.Get()
		writeJSON(w, 200, map[string]any{"app": a, "repos": repos, "ext": ext, "hub": s.hubURL(r), "key": c.AccessKey, "tls": c.TLS})
	case len(rest) == 0 && r.Method == "DELETE":
		s.store.Update(func(st *State) {
			for i, x := range st.Apps {
				if x.ID == a.ID {
					st.Apps = append(st.Apps[:i], st.Apps[i+1:]...)
					break
				}
			}
			if !s.sync.mu.TryLock() { // prune later if a sync runs
				return
			}
			defer s.sync.mu.Unlock()
			s.sync.pruneLocked(st)
		})
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(rest) == 0 && r.Method == "PUT":
		var in struct {
			Name    *string   `json:"name"`
			Recipe  *string   `json:"recipe"`
			Command *string   `json:"command"`
			Keep    *int      `json:"keep"`
			Archs   *[]string `json:"archs"`
			Exclude *string   `json:"exclude"`
			Pre     *bool     `json:"prerelease"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.Exclude != nil && *in.Exclude != "" {
			if _, err := regexp.Compile(*in.Exclude); err != nil {
				writeJSON(w, 400, map[string]string{"error": "regex نامعتبر: " + err.Error()})
				return
			}
		}
		if in.Command != nil && strings.ContainsAny(*in.Command, "\r\n") {
			writeJSON(w, 400, map[string]string{"error": "دستور نصب باید یک خط باشد"})
			return
		}
		s.store.Update(func(st *State) {
			p := st.App(a.ID)
			if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
				p.Name = strings.TrimSpace(*in.Name)
			}
			if in.Recipe != nil {
				p.Recipe = strings.ReplaceAll(*in.Recipe, "\r\n", "\n")
			}
			if in.Command != nil {
				p.Command = strings.TrimSpace(*in.Command)
			}
			if in.Keep != nil && *in.Keep >= 0 && *in.Keep <= 50 {
				p.Keep = *in.Keep
			}
			if in.Archs != nil {
				p.Archs = nil
				for _, x := range *in.Archs {
					if contains(validArchs, x) {
						p.Archs = append(p.Archs, x)
					}
				}
			}
			if in.Exclude != nil {
				p.Exclude = *in.Exclude
			}
			if in.Pre != nil {
				p.Pre = *in.Pre
			}
		})
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(rest) == 1 && rest[0] == "sync" && r.Method == "POST":
		if err := s.sync.Start([]string{a.ID}); err != nil {
			writeJSON(w, 409, map[string]string{"error": "همگام‌سازی دیگری در جریان است"})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(rest) == 1 && rest[0] == "script" && r.Method == "GET":
		// original install script as mirrored (for the recipe editor)
		u := firstURL(a.Command)
		if u == "" {
			writeJSON(w, 404, map[string]string{"error": "no script"})
			return
		}
		body, err := s.mirroredScript(u)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": "اسکریپت هنوز آینه نشده: " + err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"url": u, "script": string(body)})
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

// mirroredScript reads an install script from the mirror (raw or ext).
func (s *Server) mirroredScript(u string) ([]byte, error) {
	c := classifyURL(u)
	switch c.Kind {
	case "raw":
		rp, key := s.emu.repo(c.Owner, c.Repo)
		if rp == nil {
			return nil, os.ErrNotExist
		}
		if p, err := safeJoin(repoDir(s.data, key), "raw", tagDir(c.Raw.Ref), c.Raw.Path); err == nil && fileExists(p) {
			return os.ReadFile(p)
		}
		tb := s.emu.findTarball(key, rp, c.Raw.Ref)
		if tb == "" {
			tb = s.emu.findTarball(key, rp, "refs/heads/"+rp.DefaultBranch)
		}
		b, _, err := readFromTarball(tb, c.Raw.Path)
		return b, err
	case "ext":
		return os.ReadFile(extPath(s.data, u))
	}
	return nil, os.ErrNotExist
}

func (s *Server) status(r *http.Request) map[string]any {
	st := s.store.Snapshot()
	s.diskMu.Lock()
	if time.Since(s.diskAt) > time.Minute {
		s.diskN, s.diskAt = dirSize(s.data), time.Now()
	}
	used := s.diskN
	s.diskMu.Unlock()
	assets := 0
	for _, rp := range st.Repos {
		for _, rel := range rp.Releases {
			assets += len(rel.Assets)
		}
	}
	c := s.cfg.Get()
	return map[string]any{"version": version, "hub": s.hubURL(r), "sync": st.Sync, "disk_used": used,
		"disk_free": diskFree(s.data), "apps": len(st.Apps), "repos": len(st.Repos), "assets": assets,
		"direct_sync": c.DirectSync, "sync_hour": c.SyncHour, "now": time.Now()}
}

// ---------- public: install wrapper and downloads page ----------

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/i/"), "/"), "/")
	var a *App
	var st *State
	st = s.store.Snapshot()
	a = st.App(parts[0])
	if a == nil {
		http.Error(w, "echo 'sahand-mirror: unknown app "+strings.ReplaceAll(parts[0], "'", "")+"' >&2; exit 1", 404)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if len(parts) == 2 && parts[1] == "recipe" {
		if a.Recipe == "" {
			http.Error(w, "no recipe", 404)
			return
		}
		io.WriteString(w, a.Recipe)
		return
	}
	q := r.URL.Query()
	c := s.cfg.Get()
	o := wrapperOpts{Hub: s.hubURL(r), App: a, Key: c.AccessKey, AptHint: c.AptMirrorHint, Env: map[string]string{}}
	if c.TLS && strings.HasPrefix(o.Hub, "https://") {
		o.CAPEM = string(s.ca.CertPEM)
	}
	if v := q.Get("v"); v != "" && a.Repo != "" && validTag(v) {
		o.PinRepo, o.PinTag = a.Repo, v
	}
	if args := q.Get("args"); args != "" {
		o.Args = strings.Fields(args)
	}
	o.TraceAll = q.Get("trace") == "1"
	for k, v := range q {
		if k != "v" && k != "args" && k != "k" && k != "trace" && len(v) > 0 {
			o.Env[k] = v[0]
		}
	}
	for _, u := range a.Externals {
		if e := st.Ext[u]; e != nil && e.Mirror && !e.Fetched.IsZero() {
			o.Externs = append(o.Externs, u)
		}
	}
	io.WriteString(w, buildWrapper(o))
}

func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request) {
	st := s.store.Snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	b := &strings.Builder{}
	b.WriteString(`<!doctype html><html lang="fa" dir="rtl"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>دانلودها</title><link rel="stylesheet" href="/style.css"><body class="dl"><h1>دانلودهای هاب</h1><p class="muted">برای گوشی: فایل‌های ‎.apk‎ را مستقیم دانلود و نصب کنید. <a href="/ca.crt">گواهی CA داخلی</a></p>`)
	keys := make([]string, 0, len(st.Repos))
	for k := range st.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	apkOnly := r.URL.Query().Get("apk") == "1"
	for _, k := range keys {
		rp := st.Repos[k]
		if len(rp.Releases) == 0 {
			continue
		}
		fmt.Fprintf(b, "<section class=card><h2 dir=ltr>%s</h2>", html.EscapeString(rp.Name))
		for _, rel := range rp.Releases {
			var rows strings.Builder
			for _, a := range rel.Assets {
				if apkOnly && !strings.HasSuffix(strings.ToLower(a.Name), ".apk") {
					continue
				}
				u := "/gh/web/" + rp.Name + "/releases/download/" + url.PathEscape(rel.Tag) + "/" + url.PathEscape(a.Name)
				fmt.Fprintf(&rows, `<li dir=ltr><a href="%s">%s</a> <span class=muted>%s</span></li>`, html.EscapeString(u), html.EscapeString(a.Name), humanBytes(a.Size))
			}
			if rows.Len() > 0 {
				fmt.Fprintf(b, "<h3 dir=ltr>%s</h3><ul>%s</ul>", html.EscapeString(rel.Tag), rows.String())
			}
		}
		b.WriteString("</section>")
	}
	b.WriteString("</body></html>")
	io.WriteString(w, b.String())
}

// ---------- control socket (CLI -> running service) ----------

func (s *Server) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		r.Host = "localhost"
		writeJSON(w, 200, s.status(r))
	})
	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") == "1" {
			if err := s.sync.Run(r.Context(), nil); err != nil {
				writeJSON(w, 200, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		if err := s.sync.Start(nil); err != nil {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/export", func(w http.ResponseWriter, r *http.Request) {
		s.busy.Lock()
		defer s.busy.Unlock()
		m, err := ExportBundleFile(r.URL.Query().Get("path"), s.data, s.store)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "files": len(m.Files)})
	})
	mux.HandleFunc("/import", func(w http.ResponseWriter, r *http.Request) {
		s.busy.Lock()
		defer s.busy.Unlock()
		m, err := ImportBundle(r.URL.Query().Get("path"), s.data, s.store)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "files": len(m.Files), "apps": m.Apps})
	})
	return mux
}

func controlSock(data string) string { return filepath.Join(data, "control.sock") }

func (s *Server) Run(ctx context.Context) error {
	c := s.cfg.Get()
	sock := controlSock(s.data)
	os.Remove(sock)
	if ln, err := net.Listen("unix", sock); err == nil {
		os.Chmod(sock, 0o600)
		go http.Serve(ln, s.controlHandler())
		defer os.Remove(sock)
	} else {
		log.Printf("control socket: %v", err)
	}
	go s.sync.scheduler(ctx)
	srv := &http.Server{Addr: c.Listen, Handler: s.Handler(), ReadHeaderTimeout: 20 * time.Second, IdleTimeout: 120 * time.Second}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	var err error
	if c.TLS {
		hosts := []string{"localhost", "127.0.0.1"}
		if u, perr := url.Parse(c.PublicURL); perr == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
		tc, terr := s.ca.TLSConfig(hosts)
		if terr != nil {
			return terr
		}
		srv.TLSConfig = tc
		log.Printf("sahand-mirror %s listening on https %s (public %s)", version, c.Listen, c.PublicURL)
		err = srv.ListenAndServeTLS("", "")
	} else {
		log.Printf("sahand-mirror %s listening on http %s (public %s)", version, c.Listen, c.PublicURL)
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

var validArchs = []string{"amd64", "arm64", "armv7", "386", "other", "otheros"}
