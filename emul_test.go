package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeTarball(t *testing.T, path string, files map[string]string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "r-1/", Typeflag: tar.TypeDir, Mode: 0o755})
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: "r-1/" + n, Size: int64(len(c)), Mode: 0o644, Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture: Owner/Repo with releases v2 (latest) and v1, tarballs and an external file.
func fixture(t *testing.T) (*Server, string) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	c := defaultConfig()
	c.DataDir = filepath.Join(dir, "data")
	c.PasswordHash = hashPassword("secret-pass")
	if err := saveConfigFile(cfgFile, c); err != nil {
		t.Fatal(err)
	}
	cs, _ := NewConfigStore(cfgFile)
	s, err := NewServer(cs)
	if err != nil {
		t.Fatal(err)
	}
	rd := repoDir(c.DataDir, "owner/repo")
	os.MkdirAll(filepath.Join(rd, "releases", "v2"), 0o755)
	os.WriteFile(filepath.Join(rd, "releases", "v2", "app-linux-amd64.tar.gz"), []byte("BINARY-V2"), 0o644)
	os.WriteFile(filepath.Join(rd, "releases", "v2", "app-linux-amd64.tar.gz.sha256"), []byte("abc  app-linux-amd64.tar.gz\n"), 0o644)
	makeTarball(t, filepath.Join(rd, "src", "heads", "main.tar.gz"), map[string]string{"install.sh": "#!/bin/bash\necho main\n", "x-ui.sh": "main-xui"})
	makeTarball(t, filepath.Join(rd, "src", "tags", "v2.tar.gz"), map[string]string{"x-ui.sh": "v2-xui"})
	os.MkdirAll(filepath.Join(c.DataDir, "ext"), 0o755)
	os.WriteFile(extPath(c.DataDir, "https://get.acme.sh"), []byte("#!/bin/sh\necho acme\n"), 0o644)
	now := time.Now()
	s.store.Update(func(st *State) {
		st.Apps = []*App{{ID: "app", Repo: "Owner/Repo", Kind: "repo", Repos: []string{"owner/repo"}, Externals: []string{"https://get.acme.sh"},
			Command: "bash <(curl -Ls https://raw.githubusercontent.com/Owner/Repo/main/install.sh)"}}
		st.Repos["owner/repo"] = &Repo{Name: "Owner/Repo", DefaultBranch: "main", Latest: "v2", Releases: []Release{
			{Tag: "v2", Name: "Version 2", Published: now, Source: true, Assets: []Asset{{Name: "app-linux-amd64.tar.gz", Size: 9}, {Name: "app-linux-amd64.tar.gz.sha256", Size: 28}}},
			{Tag: "v1", Published: now.Add(-time.Hour)}}}
		st.Ext["https://get.acme.sh"] = &Ext{URL: "https://get.acme.sh", Mirror: true, Fetched: now}
	})
	return s, c.DataDir
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestGitHubEmulation(t *testing.T) {
	s, _ := fixture(t)
	h := s.Handler()

	w := get(t, h, "/gh/api/repos/owner/REPO/releases/latest")
	if w.Code != 200 {
		t.Fatalf("latest: %d %s", w.Code, w.Body)
	}
	var rel apiRelease
	json.Unmarshal(w.Body.Bytes(), &rel)
	if rel.TagName != "v2" || len(rel.Assets) != 2 || rel.Assets[0].BrowserDownloadURL != "http://example.com/gh/web/Owner/Repo/releases/download/v2/app-linux-amd64.tar.gz" {
		t.Fatalf("bad release json: %+v", rel)
	}
	// the same grep|sed the 3x-ui installer uses must find the tag
	out, err := exec.Command("bash", "-c", `grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'`).Output()
	_ = out
	cmd := exec.Command("bash", "-c", `grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'`)
	cmd.Stdin = bytes.NewReader(w.Body.Bytes())
	if out, err = cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "v2" {
		t.Fatalf("tag grep: %q %v", out, err)
	}

	w = get(t, h, "/gh/web/owner/repo/releases/latest")
	if w.Code != 302 || w.Header().Get("Location") != "/gh/web/Owner/Repo/releases/tag/v2" {
		t.Fatalf("redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w = get(t, h, "/gh/web/Owner/Repo/releases/download/v2/app-linux-amd64.tar.gz"); w.Body.String() != "BINARY-V2" {
		t.Fatalf("download: %d %q", w.Code, w.Body)
	}
	if w = get(t, h, "/gh/web/Owner/Repo/releases/latest/download/app-linux-amd64.tar.gz.sha256"); w.Code != 200 {
		t.Fatalf("latest/download: %d", w.Code)
	}
	if w = get(t, h, "/gh/raw/Owner/Repo/v2/x-ui.sh"); w.Body.String() != "v2-xui" {
		t.Fatalf("raw tag: %q", w.Body)
	}
	if w = get(t, h, "/gh/raw/Owner/Repo/refs/heads/main/x-ui.sh"); w.Body.String() != "main-xui" {
		t.Fatalf("raw refs/heads: %q", w.Body)
	}
	if w = get(t, h, "/gh/raw/owner/repo/master/install.sh"); w.Code != 200 || w.Header().Get("X-Mirror-Fallback") != "main" {
		t.Fatalf("raw fallback: %d", w.Code)
	}
	if w = get(t, h, "/gh/raw/owner/repo/v1/x-ui.sh"); w.Code != 404 {
		t.Fatalf("unmirrored tag must 404, got %d", w.Code)
	}
	if w = get(t, h, "/gh/web/owner/repo/archive/refs/heads/main.tar.gz"); w.Code != 200 {
		t.Fatalf("archive: %d", w.Code)
	}
	if w = get(t, h, "/gh/codeload/owner/repo/tar.gz/refs/tags/v2"); w.Code != 200 {
		t.Fatalf("codeload: %d", w.Code)
	}
	if w = get(t, h, "/ext?u=https%3A%2F%2Fget.acme.sh"); !strings.Contains(w.Body.String(), "echo acme") {
		t.Fatalf("ext: %d %q", w.Code, w.Body)
	}
	var list []apiRelease
	json.Unmarshal(get(t, h, "/gh/api/repos/owner/repo/releases").Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("releases list: %d", len(list))
	}
}

func TestTraversal(t *testing.T) {
	s, data := fixture(t)
	os.WriteFile(filepath.Join(data, "secret.txt"), []byte("TOPSECRET"), 0o644)
	h := s.Handler()
	for _, p := range []string{
		"/gh/web/owner/repo/releases/download/v2/..%2F..%2F..%2F..%2Fsecret.txt",
		"/gh/web/owner/repo/releases/download/../../../../secret.txt",
		"/gh/raw/owner/repo/main/../../../../secret.txt",
		"/gh/raw/owner/repo/..%2F..%2F..%2F/secret.txt",
		"/gh/web/..%2F..%2F/repo/releases/latest",
		"/gh/web/owner/repo/archive/..%2F..%2F..%2Fsecret.txt",
		"/ext?u=..%2Fsecret.txt",
	} {
		w := get(t, h, p)
		if strings.Contains(w.Body.String(), "TOPSECRET") || w.Code == 200 {
			t.Errorf("%s leaked or 200 (%d)", p, w.Code)
		}
	}
	if _, err := safeJoin("/data", "a/../../etc/passwd"); err == nil {
		t.Error("safeJoin allowed traversal")
	}
	if _, err := safeJoin("/data", "/etc/passwd"); err == nil {
		t.Error("safeJoin allowed absolute")
	}
	if p, err := safeJoin("/data", "repos/a/b"); err != nil || p != "/data/repos/a/b" {
		t.Error("safeJoin rejected a good path")
	}
}

func TestAuthAndAccessKey(t *testing.T) {
	s, _ := fixture(t)
	h := s.Handler()
	if w := get(t, h, "/api/status"); w.Code != 401 {
		t.Fatalf("status without login: %d", w.Code)
	}
	login := func(pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"admin","password":"`+pw+`"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := login("wrong"); w.Code != 401 {
		t.Fatalf("bad login: %d", w.Code)
	}
	w := login("secret-pass")
	if w.Code != 200 {
		t.Fatalf("login: %d", w.Code)
	}
	ck := w.Result().Cookies()[0]
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie flags")
	}
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(ck)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status with login: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"companion_cmd": "bash \u003c(curl -Ls 'http`) {
		t.Fatalf("status lacks companion_cmd: %s", w.Body.String())
	}
	s.cfg.Update(func(c *Config) { c.AccessKey = "k3y-123456" })
	if w := get(t, h, "/gh/api/repos/owner/repo/releases/latest"); w.Code != 401 {
		t.Fatalf("access key not enforced: %d", w.Code)
	}
	if w := get(t, h, "/gh/api/repos/owner/repo/releases/latest?k=k3y-123456"); w.Code != 200 {
		t.Fatalf("access key rejected: %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/i/app", nil)
	req.Header.Set("X-Mirror-Key", "k3y-123456")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "__SM_KEY='k3y-123456'") {
		t.Fatalf("wrapper with header key: %d", w.Code)
	}
}

func TestBundleRoundTrip(t *testing.T) {
	s, data := fixture(t)
	var buf bytes.Buffer
	m, err := ExportBundle(&buf, data, s.store.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) < 6 {
		t.Fatalf("manifest has %d files", len(m.Files))
	}
	dir := t.TempDir()
	bf := filepath.Join(dir, "b.tar.gz")
	os.WriteFile(bf, buf.Bytes(), 0o644)
	data2 := filepath.Join(dir, "data2")
	st2, _ := OpenStore(data2, false)
	if _, err := ImportBundle(bf, data2, st2); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(repoDir(data2, "owner/repo"), "releases", "v2", "app-linux-amd64.tar.gz"))
	if err != nil || string(b) != "BINARY-V2" {
		t.Fatalf("asset not imported: %v", err)
	}
	if st2.Snapshot().App("app") == nil || st2.Snapshot().Sync.LastImport.IsZero() {
		t.Fatal("state not merged")
	}
	// tamper: change one byte of a file inside the bundle -> import must fail
	tampered := tamper(t, buf.Bytes(), "BINARY-V2", "BINARY-XX")
	os.WriteFile(bf, tampered, 0o644)
	if _, err := ImportBundle(bf, filepath.Join(dir, "data3"), st2); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered bundle accepted: %v", err)
	}
}

func tamper(t *testing.T, b []byte, from, to string) []byte {
	gz, _ := gzip.NewReader(bytes.NewReader(b))
	raw, _ := io.ReadAll(gz)
	raw = bytes.Replace(raw, []byte(from), []byte(to), 1)
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	w.Write(raw)
	w.Close()
	return out.Bytes()
}

func TestPassword(t *testing.T) {
	h := hashPassword("hello-world")
	if !checkPassword(h, "hello-world") || checkPassword(h, "hello-worlD") || checkPassword("garbage", "x") {
		t.Fatal("password hashing broken")
	}
}
