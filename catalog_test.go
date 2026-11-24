package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestCatalogAndReadme(t *testing.T) {
	s := testServer(t, nil)
	st := s.store.Snapshot()
	if st.App(companionID) == nil {
		t.Fatal("companion app not seeded")
	}
	key := "sahand363/zimzim"
	s.store.Update(func(st *State) {
		st.Repos[key] = &Repo{Name: "sahand363/zimzim", DefaultBranch: "main", Latest: "v1.2.0",
			Releases: []Release{{Tag: "v1.2.0", Published: time.Now(), Assets: []Asset{{Name: "a.tar.gz", Size: 100}}}}}
	})
	src := filepath.Join(repoDir(s.data, key), "src", "heads")
	os.MkdirAll(src, 0o755)
	makeTarball(t, filepath.Join(src, "main.tar.gz"), map[string]string{"README.md": "# ZimZim\nhello"})

	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req("/catalog"))
	if w.Code != 200 {
		t.Fatalf("catalog %d %s", w.Code, w.Body)
	}
	var cat struct {
		Companion string        `json:"companion"`
		Apps      []CatalogItem `json:"apps"`
	}
	json.Unmarshal(w.Body.Bytes(), &cat)
	if !strings.Contains(cat.Companion, "http://10.0.0.5:8686/companion") {
		t.Fatalf("companion cmd %q", cat.Companion)
	}
	var z *CatalogItem
	for i := range cat.Apps {
		if cat.Apps[i].ID == "zimzim" {
			z = &cat.Apps[i]
		}
	}
	if z == nil || z.Latest != "v1.2.0" || !z.Readme || z.Assets != 1 || !strings.Contains(z.Install, "/i/zimzim") {
		t.Fatalf("bad item %+v", z)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req("/catalog/zimzim/readme"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "# ZimZim") {
		t.Fatalf("readme %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req("/companion"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "has not been mirrored yet") || !strings.Contains(w.Body.String(), "exit 1") {
		t.Fatalf("unmirrored companion: %d %s", w.Code, w.Body)
	}
	for _, r := range w.Body.String() {
		if r > 127 {
			t.Fatal("non-ASCII in terminal output")
		}
	}
	s.store.Update(func(st *State) {
		st.Repos["sahand363/sahand-mirror-companion"] = &Repo{Name: "sahand363/sahand-mirror-companion", DefaultBranch: "main"}
	})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req("/companion"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "MIRROR_URL=") {
		t.Fatalf("companion wrapper %d", w.Code)
	}
}

func TestSeedMigrationOnce(t *testing.T) {
	st := &State{Repos: map[string]*Repo{}, Apps: []*App{{ID: "zimzim"}}}
	if !st.migrateSeeds() || st.App(companionID) == nil {
		t.Fatal("companion should be added")
	}
	if st.migrateSeeds() {
		t.Fatal("second migration changed state")
	}
	// the companion ships with the hub: it always comes back
	st.Apps = st.Apps[:1]
	if !st.migrateSeeds() || st.App(companionID) == nil {
		t.Fatal("companion not restored")
	}
	if st.App("loadinme") != nil {
		t.Fatal("old default re-added")
	}
}

func req(p string) *http.Request {
	r := httptest.NewRequest("GET", p, nil)
	r.Host = "10.0.0.5:8686"
	return r
}

func TestBundledCompanion(t *testing.T) {
	s := testServer(t, nil)
	mk := func(tag, body string) fstest.MapFS {
		return fstest.MapFS{
			"companion/VERSION":    {Data: []byte(tag + "\n")},
			"companion/README.txt": {Data: []byte("x")},
			"companion/install.sh": {Data: []byte("#!/bin/bash\necho companion " + tag + "\n")},
			"companion/README.md":  {Data: []byte("# Companion")},
			"companion/sahand-mirror-companion-linux-amd64.tar.gz": {Data: []byte(body)},
		}
	}
	tag, err := importCompanion(s.store, s.data, mk("v0.1.2", "BIN-012"))
	if err != nil || tag != "v0.1.2" {
		t.Fatalf("import: %q %v", tag, err)
	}
	if tag, _ := importCompanion(s.store, s.data, mk("v0.1.2", "BIN-012")); tag != "" {
		t.Fatal("re-imported the same version")
	}
	if tag, _ := importCompanion(s.store, s.data, mk("v0.1.1", "OLD")); tag != "" {
		t.Fatal("older bundle replaced newer")
	}
	h := s.Handler()
	check := func(p string, code int, want string) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req(p))
		if w.Code != code || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	check("/gh/raw/sahand363/sahand-mirror-companion/main/install.sh", 200, "echo companion v0.1.2")
	check("/gh/web/sahand363/sahand-mirror-companion/releases/latest/download/sahand-mirror-companion-linux-amd64.tar.gz", 200, "BIN-012")
	check("/companion", 200, "MIRROR_URL=")
	check("/catalog/sahand-mirror-companion/readme", 200, "# Companion")
	if tag, _ := importCompanion(s.store, s.data, mk("v0.1.10", "BIN-0110")); tag != "v0.1.10" {
		t.Fatal("newer bundle not imported")
	}
	check("/gh/web/sahand363/sahand-mirror-companion/releases/latest/download/sahand-mirror-companion-linux-amd64.tar.gz", 200, "BIN-0110")
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"v0.1.2", "v0.1.10"}, {"", "v0.1.0"}, {"v0.9.9", "v1.0.0"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Fatal(c)
		}
	}
}
