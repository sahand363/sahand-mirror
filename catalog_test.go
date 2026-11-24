package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// user deletes it; must not come back
	st.Apps = st.Apps[:1]
	if st.migrateSeeds() || st.App(companionID) != nil {
		t.Fatal("deleted companion re-added")
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
