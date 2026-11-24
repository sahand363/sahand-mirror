package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The release workflow downloads the latest sahand-mirror-companion release
// into companion/ before building, so every hub ships with it. Hub and
// companion work together: the hub must be able to serve the companion even
// if it was never synced (or the international link is already gone).
//
//go:embed companion
var embeddedCompanion embed.FS

const companionRepo = "sahand363/sahand-mirror-companion"

// importCompanion copies the bundled companion release into the mirror store
// when the hub has none or an older one. Returns the imported tag ("" = none).
func importCompanion(store *Store, data string, fsys fs.FS) (string, error) {
	vb, err := fs.ReadFile(fsys, "companion/VERSION")
	tag := strings.TrimSpace(string(vb))
	if err != nil || tag == "" || !validTag(tag) {
		return "", nil // dev build without a bundled companion
	}
	key := strings.ToLower(companionRepo)
	var cur *Repo
	store.View(func(st *State) {
		if r := st.Repos[st.canon(key)]; r != nil {
			c := *r
			cur = &c
		}
	})
	if cur != nil {
		if versionLess(tag, cur.Latest) {
			return "", nil // hub already synced a newer one from GitHub
		}
		if lr := cur.release(tag); lr != nil && assetsPresent(data, key, lr) {
			return "", nil // already in place
		}
	}

	ents, err := fs.ReadDir(fsys, "companion")
	if err != nil {
		return "", err
	}
	rd := repoDir(data, key)
	relDir := filepath.Join(rd, "releases", tagDir(tag))
	if err := os.MkdirAll(relDir, 0o755); err != nil {
		return "", err
	}
	rel := Release{Tag: tag, Name: "sahand-mirror-companion " + tag, Published: time.Now()}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || n == "VERSION" || n == "README.txt" || !validTag(n) {
			continue
		}
		b, err := fs.ReadFile(fsys, "companion/"+n)
		if err != nil {
			return "", err
		}
		switch n {
		case "install.sh", "README.md":
			// served as raw files of the default branch (the hub's /companion
			// runs raw main/install.sh; the catalog shows README.md)
			if err := os.MkdirAll(filepath.Join(rd, "raw", "main"), 0o755); err != nil {
				return "", err
			}
			if err := writeFileAtomic(filepath.Join(rd, "raw", "main", n), b, 0o644); err != nil {
				return "", err
			}
			if n == "README.md" {
				continue
			}
		}
		if err := writeFileAtomic(filepath.Join(relDir, n), b, 0o644); err != nil {
			return "", err
		}
		h := sha256.Sum256(b)
		rel.Assets = append(rel.Assets, Asset{Name: n, Size: int64(len(b)), SHA256: hex.EncodeToString(h[:])})
	}
	if len(rel.Assets) == 0 {
		return "", nil
	}
	err = store.Update(func(st *State) {
		k := st.canon(key)
		rp := st.Repos[k]
		if rp == nil {
			rp = &Repo{Name: companionRepo, DefaultBranch: "main"}
			st.Repos[k] = rp
		}
		if rp.DefaultBranch == "" {
			rp.DefaultBranch = "main"
		}
		out := []Release{rel}
		for _, r := range rp.Releases {
			if r.Tag != tag {
				out = append(out, r)
			}
		}
		rp.Releases, rp.Latest = out, tag
		for _, f := range []string{"main/install.sh", "main/README.md"} {
			if !contains(rp.Raw, f) {
				rp.Raw = append(rp.Raw, f)
			}
		}
		if rp.LastSync.IsZero() {
			rp.LastSync = time.Now()
		}
		if a := st.App(companionID); a == nil {
			st.Apps = append(st.Apps, defaultCompanionApp())
		}
	})
	return tag, err
}

func assetsPresent(data, key string, rel *Release) bool {
	for _, a := range rel.Assets {
		if !fileExists(filepath.Join(repoDir(data, key), "releases", tagDir(rel.Tag), a.Name)) {
			return false
		}
	}
	return len(rel.Assets) > 0
}

func defaultCompanionApp() *App {
	for _, a := range defaultApps() {
		if a.ID == companionID {
			return a
		}
	}
	return nil
}

// versionLess compares vX.Y.Z tags numerically; non-numeric parts compare as text.
func versionLess(a, b string) bool {
	if a == "" {
		return b != ""
	}
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		xn, e1 := strconv.Atoi(x)
		yn, e2 := strconv.Atoi(y)
		if e1 == nil && e2 == nil {
			if xn != yn {
				return xn < yn
			}
			continue
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// companionWatch keeps the companion current while the hub has internet:
// a light sync of just that app every hour (the daily full sync still runs).
func (s *Server) companionWatch(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if !s.cfg.Get().DirectSync {
				continue
			}
			if err := s.sync.Start([]string{companionID}); err != nil && err != errBusy {
				log.Printf("companion update check: %v", err)
			}
		}
	}
}
