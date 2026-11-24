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
	aptMu  sync.Mutex // one apt prefetch at a time
	aiMu   sync.Mutex
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
	s.sync.fallbackDocs = s.mirrorDocs
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
	mux.HandleFunc("/catalog", s.public(s.handleCatalog))
	mux.HandleFunc("/catalog/", s.public(s.handleCatalogItem))
	mux.HandleFunc("/companion", s.public(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/i/" + companionID
		s.handleInstall(w, r2)
	}))
	mux.HandleFunc("/myip", s.public(s.handleMyIP))
	mux.HandleFunc("/apt/", s.public(s.handleAptPath))
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
	h := logRequests(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// forward-proxy requests (apt with Acquire::http::Proxy=http://HUB/)
		if r.Method == http.MethodConnect || strings.HasPrefix(r.RequestURI, "http://") || strings.HasPrefix(r.RequestURI, "https://") {
			s.handleProxy(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// handleMyIP answers IP-detection requests with the client's address.
func (s *Server) handleMyIP(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.cfg.Get().TrustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); net.ParseIP(first) != nil {
				ip = first
			}
		} else if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(xr) != nil {
			ip = xr
		}
	}
	if strings.Contains(r.URL.RawQuery, "json") {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ip":"`+ip+`"}`)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, ip+"\n")
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
		if strings.HasPrefix(p, "/gh/") || strings.HasPrefix(p, "/i/") || p == "/ext" || p == "/d" || p == "/myip" {
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
			if got == "" {
				if _, pw, ok := r.BasicAuth(); ok {
					got = pw // apt sources: http://sm:KEY@hub/apt/...
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
		writeJSON(w, 200, map[string]any{"public_url": c.PublicURL, "listen": c.Listen, "direct_sync": c.DirectSync, "companion_cmd": s.companionCmd(r),
			"sync_hour": c.SyncHour, "keep": c.Keep, "token_set": c.GitHubToken != "", "access_key": c.AccessKey,
			"tls": c.TLS, "apt_mirror_hint": c.AptMirrorHint, "admin_user": c.AdminUser, "max_upload_mb": c.MaxUploadMB,
			"apt_proxy": !c.AptProxyOff, "apt_targets": aptTargetsOrDefault(c), "apt_releases": aptReleases,
			"apt_upstream": aptUpstreamView(c), "apt_internal": c.AptInternal, "apt_families": aptFamilies,
			"trust_xff": c.TrustXFF, "ai_provider": c.AIProvider, "ai_base_url": c.AIBaseURL, "ai_model": c.AIModel,
			"ai_key_set": c.AIKey != "", "apt_snippet": s.aptSnippet(r), "hub": s.hubURL(r)})
	case p == "settings" && r.Method == "PUT":
		var in struct {
			PublicURL     *string            `json:"public_url"`
			DirectSync    *bool              `json:"direct_sync"`
			SyncHour      *int               `json:"sync_hour"`
			Keep          *int               `json:"keep"`
			GitHubToken   *string            `json:"github_token"`
			AccessKey     *string            `json:"access_key"`
			TLS           *bool              `json:"tls"`
			AptMirrorHint *string            `json:"apt_mirror_hint"`
			MaxUploadMB   *int64             `json:"max_upload_mb"`
			AptProxy      *bool              `json:"apt_proxy"`
			AptTargets    *[]string          `json:"apt_targets"`
			AptUpstream   *map[string]string `json:"apt_upstream"`
			AptInternal   *map[string]string `json:"apt_internal"`
			TrustXFF      *bool              `json:"trust_xff"`
			AIProvider    *string            `json:"ai_provider"`
			AIBaseURL     *string            `json:"ai_base_url"`
			AIModel       *string            `json:"ai_model"`
			AIKey         *string            `json:"ai_key"`
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
		for _, mp := range []*map[string]string{in.AptUpstream, in.AptInternal} {
			if mp == nil {
				continue
			}
			for fam, v := range *mp {
				if !contains(aptFamilies, fam) {
					bad("unknown apt family " + fam)
					return
				}
				if v = strings.TrimSpace(v); v != "" {
					if u, err := url.Parse(v); err != nil || u.Scheme != "http" || u.Host == "" {
						bad("آدرس میرور apt باید http:// باشد: " + v)
						return
					}
				}
			}
		}
		if in.AIProvider != nil && *in.AIProvider != "" && *in.AIProvider != "gemini" && *in.AIProvider != "openai" {
			bad("ai_provider: gemini یا openai")
			return
		}
		if in.AIBaseURL != nil && *in.AIBaseURL != "" {
			if u, err := url.Parse(*in.AIBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				bad("آدرس پایهٔ AI نامعتبر است")
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
			if in.AptProxy != nil {
				c.AptProxyOff = !*in.AptProxy
			}
			if in.AptTargets != nil {
				c.AptTargets = []string{}
				for _, t := range *in.AptTargets {
					if contains(defaultAptTargets(), t) {
						c.AptTargets = append(c.AptTargets, t)
					}
				}
			}
			clean := func(m map[string]string) map[string]string {
				out := map[string]string{}
				for k, v := range m {
					if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
						out[k] = v
					}
				}
				return out
			}
			if in.AptUpstream != nil {
				c.AptUpstream = clean(*in.AptUpstream)
				for k, v := range c.AptUpstream {
					if v == defaultAptUpstream[k] {
						delete(c.AptUpstream, k)
					}
				}
			}
			if in.AptInternal != nil {
				c.AptInternal = clean(*in.AptInternal)
			}
			if in.TrustXFF != nil {
				c.TrustXFF = *in.TrustXFF
			}
			set(&c.AIProvider, in.AIProvider)
			set(&c.AIBaseURL, in.AIBaseURL)
			set(&c.AIModel, in.AIModel)
			if in.AIKey != nil && *in.AIKey != "" { // write-only; "-" clears
				c.AIKey = strings.TrimSpace(*in.AIKey)
				if c.AIKey == "-" {
					c.AIKey = ""
				}
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
	case p == "apt" && r.Method == "GET":
		st := s.store.Snapshot()
		writeJSON(w, 200, map[string]any{"apt": st.Apt, "cache_bytes": s.aptCacheSize(), "snippet": s.aptSnippet(r)})
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
		items, sum := evaluateReadiness(a, st, c)
		var targets []string
		for _, t := range aptTargetsFor(c, a.Archs) {
			targets = append(targets, t.Key())
		}
		writeJSON(w, 200, map[string]any{"app": a, "repos": repos, "ext": ext, "hub": s.hubURL(r), "key": c.AccessKey, "tls": c.TLS,
			"readiness": items, "summary": sum, "apt_packages": aptPackagesOf(a), "apt_targets": targets, "apt": st.Apt,
			"ai_enabled": c.AIProvider != "" && c.AIKey != "", "apt_proxy": !c.AptProxyOff})
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
			Name     *string   `json:"name"`
			Recipe   *string   `json:"recipe"`
			Command  *string   `json:"command"`
			Keep     *int      `json:"keep"`
			Archs    *[]string `json:"archs"`
			Exclude  *string   `json:"exclude"`
			Pre      *bool     `json:"prerelease"`
			Disabled *[]string `json:"disabled_repos"`
			AptExtra *[]string `json:"apt_extra"`
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
			if in.Disabled != nil {
				p.Disabled = nil
				for _, k := range *in.Disabled {
					k = strings.ToLower(strings.TrimSpace(k))
					if contains(p.Repos, k) && !strings.EqualFold(st.canon(strings.ToLower(p.Repo)), k) && !contains(p.Disabled, k) {
						p.Disabled = append(p.Disabled, k)
					}
				}
			}
			if in.AptExtra != nil {
				p.AptExtra = nil
				for _, x := range *in.AptExtra {
					if x = strings.TrimSpace(x); pkgNameRe.MatchString(x) && !contains(p.AptExtra, x) {
						p.AptExtra = append(p.AptExtra, x)
					}
				}
			}
			if in.Disabled != nil && !s.sync.mu.TryLock() {
				return
			} else if in.Disabled != nil {
				defer s.sync.mu.Unlock()
				s.sync.pruneLocked(st)
			}
		})
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(rest) == 1 && rest[0] == "sync" && r.Method == "POST":
		if err := s.sync.Start([]string{a.ID}); err != nil {
			writeJSON(w, 409, map[string]string{"error": "همگام‌سازی دیگری در جریان است"})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(rest) == 1 && rest[0] == "analyze" && r.Method == "POST":
		s.sync.Analyze(a.ID, nil)
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(rest) == 1 && rest[0] == "prefetch" && r.Method == "POST":
		var in struct {
			Packages []string `json:"packages"`
			Targets  []string `json:"targets"`
		}
		json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
		var pk []string
		for _, x := range in.Packages {
			if x = strings.TrimSpace(x); pkgNameRe.MatchString(x) && !contains(pk, x) {
				pk = append(pk, x)
			}
		}
		if len(pk) == 0 {
			pk = aptPackagesOf(a)
		}
		all := aptTargetsFor(s.cfg.Get(), a.Archs)
		var ts []AptTarget
		for _, t := range all {
			if len(in.Targets) == 0 || contains(in.Targets, t.Key()) {
				ts = append(ts, t)
			}
		}
		if len(pk) == 0 || len(ts) == 0 {
			writeJSON(w, 400, map[string]string{"error": "بسته یا هدفی انتخاب نشده"})
			return
		}
		if err := s.StartPrefetch(pk, ts); err != nil {
			writeJSON(w, 409, map[string]string{"error": "پیش‌دریافت دیگری در جریان است"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "packages": pk, "targets": len(ts)})
	case len(rest) == 1 && rest[0] == "ai" && r.Method == "POST":
		if !s.aiMu.TryLock() {
			writeJSON(w, 409, map[string]string{"error": "درخواست AI دیگری در جریان است"})
			return
		}
		defer s.aiMu.Unlock()
		docs := loadSavedScripts(s.data, a.ID)
		if docs == nil {
			docs = s.mirrorDocs(a.ID)
		}
		if len(docs) == 0 {
			writeJSON(w, 400, map[string]string{"error": "اسکریپتی برای تحلیل نیست؛ اول همگام‌سازی کنید"})
			return
		}
		st := s.store.Snapshot()
		c := s.cfg.Get()
		items, _ := evaluateReadiness(st.App(a.ID), st, c)
		ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
		defer cancel()
		ai, model, err := aiReview(ctx, c, a, docs, items)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		s.store.Update(func(st *State) {
			if p := st.App(a.ID); p != nil {
				if p.Readiness == nil {
					p.Readiness = &Readiness{}
				}
				p.Readiness.AI, p.Readiness.AIAt, p.Readiness.AIModel = ai, time.Now(), model
			}
		})
		writeJSON(w, 200, map[string]any{"ok": true, "items": len(ai), "model": model})
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
	type appReady struct {
		ID   string       `json:"id"`
		Name string       `json:"name"`
		Sum  ReadySummary `json:"summary"`
		Done bool         `json:"analyzed"`
	}
	var ready []appReady
	for _, a := range st.Apps {
		_, sum := evaluateReadiness(a, st, c)
		ready = append(ready, appReady{a.ID, a.Name, sum, a.Readiness != nil})
	}
	var apt any
	if st.Apt != nil {
		apt = map[string]any{"running": st.Apt.Running, "progress": st.Apt.Progress, "last_error": st.Apt.LastErr, "targets": len(st.Apt.Targets)}
	}
	return map[string]any{"version": version, "hub": s.hubURL(r), "companion_cmd": s.companionCmd(r), "sync": st.Sync, "disk_used": used, "readiness": ready, "apt": apt,
		"apt_proxy": !c.AptProxyOff,
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
	if !c.AptProxyOff && q.Get("apt") != "0" {
		o.AptProxy = aptProxyURL(o.Hub, c.AccessKey)
	}
	o.AptHTTPS = q.Get("apt_https")
	for k, v := range q {
		if k != "v" && k != "args" && k != "k" && k != "trace" && k != "apt" && k != "apt_https" && len(v) > 0 {
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
	go func() { // apps synced by v0.1 get a readiness checklist from their mirrored scripts
		var ids []string
		s.store.View(func(st *State) {
			for _, a := range st.Apps {
				if a.Readiness == nil {
					ids = append(ids, a.ID)
				}
			}
		})
		for _, id := range ids {
			s.sync.Analyze(id, nil)
		}
	}()
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

func aptTargetsOrDefault(c Config) []string {
	if c.AptTargets == nil {
		return defaultAptTargets()
	}
	return c.AptTargets
}

func aptUpstreamView(c Config) map[string]string {
	m := map[string]string{}
	for _, f := range aptFamilies {
		m[f] = defaultAptUpstream[f]
		if v := c.AptUpstream[f]; v != "" {
			m[f] = v
		}
	}
	return m
}

// aptSnippet: commands to point a whole server permanently at the hub's apt proxy.
func (s *Server) aptSnippet(r *http.Request) string {
	c := s.cfg.Get()
	p := aptProxyURL(s.hubURL(r), c.AccessKey)
	if p == "" {
		return "# apt proxy needs the hub on http:// (HTTPS hub: point sources.list at " + s.hubURL(r) + "/apt/<host>/<path>)"
	}
	return "echo 'Acquire::http::Proxy \"" + p + "\";' > /etc/apt/apt.conf.d/99sahand-mirror && apt-get update\n" +
		"# undo: rm /etc/apt/apt.conf.d/99sahand-mirror"
}

// mirrorDocs reads an app's scripts straight from the mirror (for apps
// synced before scripts were saved).
func (s *Server) mirrorDocs(id string) []ScriptDoc {
	var a *App
	s.store.View(func(st *State) {
		if p := st.App(id); p != nil {
			c := *p
			a = &c
		}
	})
	if a == nil {
		return nil
	}
	var docs []ScriptDoc
	seen := map[string]bool{}
	try := func(u string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		if b, err := s.mirroredScript(u); err == nil && looksLikeScript(b) {
			docs = append(docs, ScriptDoc{URL: u, Body: string(b)})
		}
	}
	try(firstURL(a.Command))
	for _, u := range a.Raw {
		if strings.HasSuffix(u, ".sh") {
			try(u)
		}
	}
	for _, u := range a.Externals {
		try(u)
	}
	if a.Recipe != "" {
		docs = append(docs, ScriptDoc{URL: "recipe", Body: a.Recipe})
	}
	return docs
}

var validArchs = []string{"amd64", "arm64", "armv7", "386", "other", "otheros"}
