package main

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Public, read-only catalog for sahand-mirror-companion and other clients.

type CatalogItem struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Kind      string       `json:"kind"`
	Repo      string       `json:"repo,omitempty"`
	Command   string       `json:"command,omitempty"` // original (GitHub) install command
	Install   string       `json:"install"`           // one-line install from this hub
	Latest    string       `json:"latest,omitempty"`
	Published time.Time    `json:"published,omitempty"`
	Releases  int          `json:"releases"`
	Assets    int          `json:"assets"`
	Size      int64        `json:"size"`
	LastSync  time.Time    `json:"last_sync"`
	Error     string       `json:"error,omitempty"`
	Analyzed  bool         `json:"analyzed"`
	Ready     ReadySummary `json:"ready"`
	Readme    bool         `json:"readme"`
	Note      string       `json:"note,omitempty"`
}

var readmeNames = []string{"README.md", "readme.md", "Readme.md", "README.MD", "README.markdown", "README.rst", "README.txt", "README"}

func (s *Server) companionCmd(r *http.Request) string {
	u := strings.TrimRight(s.hubURL(r), "/") + "/companion"
	if k := s.cfg.Get().AccessKey; k != "" {
		u += "?k=" + url.QueryEscape(k)
	}
	return "bash <(curl -Ls '" + u + "')"
}

func (s *Server) installCmd(r *http.Request, id string) string {
	u := strings.TrimRight(s.hubURL(r), "/") + "/i/" + id
	if k := s.cfg.Get().AccessKey; k != "" {
		u += "?k=" + url.QueryEscape(k)
	}
	return "bash <(curl -Ls '" + u + "')"
}

// readme returns the README of the app's primary repo from the mirrored
// default-branch source snapshot (or individually stored raw files).
func (s *Server) readme(st *State, a *App) (string, []byte) {
	if a.Repo == "" {
		return "", nil
	}
	key := st.canon(strings.ToLower(a.Repo))
	rp := st.Repos[key]
	if rp == nil {
		return "", nil
	}
	branch := rp.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	for _, n := range readmeNames {
		if p, err := safeJoin(repoDir(s.data, key), "raw", tagDir(branch), n); err == nil && fileExists(p) {
			if b, err := readFileLimit(p, 2<<20); err == nil {
				return n, b
			}
		}
	}
	tb := s.emu.findTarball(key, rp, "refs/heads/"+branch)
	if tb == "" && rp.Latest != "" {
		tb = s.emu.findTarball(key, rp, rp.Latest)
	}
	if tb == "" {
		return "", nil
	}
	for _, n := range readmeNames {
		if b, _, err := readFromTarball(tb, n); err == nil {
			if len(b) > 2<<20 {
				b = b[:2<<20]
			}
			return n, b
		}
	}
	return "", nil
}

func (s *Server) catalogItem(r *http.Request, st *State, a *App, c Config) CatalogItem {
	it := CatalogItem{ID: a.ID, Name: a.Name, Kind: a.Kind, Repo: a.Repo, Command: a.Command,
		Install: s.installCmd(r, a.ID), LastSync: a.LastSync, Error: a.Error, Note: a.Note,
		Analyzed: a.Readiness != nil}
	_, it.Ready = evaluateReadiness(a, st, c)
	if a.Repo != "" {
		if rp := st.Repos[st.canon(strings.ToLower(a.Repo))]; rp != nil {
			it.Latest = rp.Latest
			it.Releases = len(rp.Releases)
			for _, rel := range rp.Releases {
				if rel.Tag == rp.Latest || (it.Latest == "" && it.Published.IsZero()) {
					it.Published = rel.Published
				}
				for _, as := range rel.Assets {
					it.Assets++
					it.Size += as.Size
				}
			}
			if it.Latest == "" && len(rp.Releases) > 0 {
				it.Latest = rp.Releases[0].Tag
			}
		}
	}
	n, _ := s.readme(st, a)
	it.Readme = n != ""
	return it
}

// GET /catalog -> {"hub","version","generated","apps":[...]}
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	st := s.store.Snapshot()
	c := s.cfg.Get()
	items := make([]CatalogItem, 0, len(st.Apps))
	for _, a := range st.Apps {
		items = append(items, s.catalogItem(r, st, a, c))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"hub": s.hubURL(r), "version": version, "generated": time.Now().UTC(),
		"last_sync": st.Sync.LastOK, "companion": s.companionCmd(r), "apps": items})
}

// GET /catalog/<id> (JSON) and /catalog/<id>/readme (text/markdown)
func (s *Server) handleCatalogItem(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/catalog/"), "/"), "/")
	st := s.store.Snapshot()
	a := st.App(parts[0])
	if a == nil {
		writeJSON(w, 404, map[string]string{"error": "unknown app"})
		return
	}
	switch {
	case len(parts) == 1:
		writeJSON(w, 200, s.catalogItem(r, st, a, s.cfg.Get()))
	case len(parts) == 2 && parts[1] == "readme":
		n, b := s.readme(st, a)
		if n == "" {
			http.Error(w, "README not mirrored", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("X-Readme-Name", n)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(b)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
