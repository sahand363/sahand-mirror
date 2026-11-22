package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Emulator serves a GitHub look-alike from the mirror so unmodified scripts work.
type Emulator struct {
	store *Store
	data  string
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func notFound(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]string{"message": "Not Found (sahand-mirror): " + msg})
}

func (e *Emulator) repo(owner, repo string) (*Repo, string) {
	key := repoKey(owner, repo)
	var rp *Repo
	e.store.View(func(st *State) {
		key = st.canon(key)
		if p := st.Repos[key]; p != nil {
			c := *p
			rp = &c
		}
	})
	return rp, key
}

// splitPath splits a cleaned URL path after prefix into segments; rejects traversal.
func splitPath(p, prefix string) ([]string, bool) {
	rest := strings.TrimPrefix(p, prefix)
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	for _, s := range segs {
		if s == ".." || s == "." || strings.ContainsRune(s, 0) {
			return nil, false
		}
	}
	return segs, true
}

// ServeHTTP handles /gh/raw, /gh/api, /gh/web, /gh/codeload, /gh/objects.
func (e *Emulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segs, ok := splitPath(r.URL.Path, "/gh/")
	if !ok || len(segs) < 1 {
		notFound(w, "bad path")
		return
	}
	switch segs[0] {
	case "raw":
		if len(segs) < 5 {
			notFound(w, "path too short")
			return
		}
		ref, rest := segs[3], segs[4:]
		if ref == "refs" && len(segs) >= 7 && (segs[4] == "heads" || segs[4] == "tags") {
			ref, rest = segs[5], segs[6:]
		}
		e.serveRaw(w, r, segs[1], segs[2], ref, strings.Join(rest, "/"))
	case "api":
		e.serveAPI(w, r, segs[1:])
	case "web":
		e.serveWeb(w, r, segs[1:])
	case "codeload": // codeload.github.com/O/R/tar.gz/refs/heads/main | /tar.gz/main
		if len(segs) < 5 || segs[3] != "tar.gz" {
			notFound(w, "only tar.gz is mirrored")
			return
		}
		ref := strings.Join(segs[4:], "/")
		e.serveArchive(w, r, segs[1], segs[2], ref)
	default:
		notFound(w, "this GitHub host is not mirrored")
	}
}

func (e *Emulator) serveWeb(w http.ResponseWriter, r *http.Request, s []string) {
	if len(s) < 3 {
		notFound(w, "not mirrored")
		return
	}
	owner, repo := s[0], strings.TrimSuffix(s[1], ".git")
	rp, key := e.repo(owner, repo)
	if rp == nil {
		notFound(w, owner+"/"+repo+" is not mirrored")
		return
	}
	rest := s[2:]
	switch {
	case rest[0] == "releases" && len(rest) == 2 && rest[1] == "latest":
		lr := rp.latestRelease()
		if lr == nil {
			notFound(w, "no releases mirrored")
			return
		}
		http.Redirect(w, r, "/gh/web/"+rp.Name+"/releases/tag/"+url.PathEscape(lr.Tag), http.StatusFound)
	case rest[0] == "releases" && len(rest) == 4 && rest[1] == "latest" && rest[2] == "download":
		lr := rp.latestRelease()
		if lr == nil {
			notFound(w, "no releases mirrored")
			return
		}
		e.serveAsset(w, r, key, lr, rest[3])
	case rest[0] == "releases" && len(rest) == 3 && rest[1] == "tag":
		rel := rp.release(rest[2])
		if rel == nil {
			notFound(w, "release "+rest[2]+" not mirrored")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s %s</title><h1>%s</h1><ul>", html.EscapeString(rp.Name), html.EscapeString(rel.Tag), html.EscapeString(rel.Name))
		for _, a := range rel.Assets {
			u := "/gh/web/" + rp.Name + "/releases/download/" + url.PathEscape(rel.Tag) + "/" + url.PathEscape(a.Name)
			fmt.Fprintf(w, `<li><a href="%s">%s</a> (%s)</li>`, html.EscapeString(u), html.EscapeString(a.Name), humanBytes(a.Size))
		}
		fmt.Fprint(w, "</ul>")
	case rest[0] == "releases" && len(rest) == 4 && rest[1] == "download":
		rel := rp.release(rest[2])
		if rel == nil {
			notFound(w, "release "+rest[2]+" not mirrored")
			return
		}
		e.serveAsset(w, r, key, rel, rest[3])
	case rest[0] == "archive" && len(rest) >= 2:
		e.serveArchive(w, r, owner, repo, strings.TrimSuffix(strings.Join(rest[1:], "/"), ".tar.gz"))
	case (rest[0] == "raw" || rest[0] == "blob") && len(rest) >= 3:
		e.serveRaw(w, r, owner, repo, rest[1], strings.Join(rest[2:], "/"))
	default:
		notFound(w, "not mirrored")
	}
}

func (e *Emulator) serveAsset(w http.ResponseWriter, r *http.Request, key string, rel *Release, name string) {
	var known bool
	for _, a := range rel.Assets {
		if a.Name == name {
			known = true
		}
	}
	if !known || !validTag(name) {
		notFound(w, "asset "+name+" not mirrored")
		return
	}
	p, err := safeJoin(repoDir(e.data, key), "releases", tagDir(rel.Tag), name)
	if err != nil {
		notFound(w, "bad name")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		notFound(w, "asset file missing")
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// serveArchive serves refs/heads/X, refs/tags/X or plain X tarballs.
func (e *Emulator) serveArchive(w http.ResponseWriter, r *http.Request, owner, repo, ref string) {
	rp, key := e.repo(owner, repo)
	if rp == nil {
		notFound(w, "repo not mirrored")
		return
	}
	ref = strings.TrimSuffix(ref, ".tar.gz")
	p := e.findTarball(key, rp, ref)
	if p == "" {
		notFound(w, "archive "+ref+" not mirrored")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		notFound(w, "archive missing")
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeContent(w, r, path.Base(ref)+".tar.gz", fi.ModTime(), f)
}

var tagLikeRe = regexp.MustCompile(`^v?\d`)

func (e *Emulator) findTarball(key string, rp *Repo, ref string) string {
	dir := repoDir(e.data, key)
	kind := ""
	if strings.HasPrefix(ref, "refs/heads/") {
		kind, ref = "heads", strings.TrimPrefix(ref, "refs/heads/")
	} else if strings.HasPrefix(ref, "refs/tags/") {
		kind, ref = "tags", strings.TrimPrefix(ref, "refs/tags/")
	}
	if ref == "HEAD" {
		ref, kind = rp.DefaultBranch, "heads"
	}
	if !validTag(strings.ReplaceAll(ref, "/", "_")) {
		return ""
	}
	for _, k := range []string{"tags", "heads"} {
		if kind != "" && kind != k {
			continue
		}
		if p := filepath.Join(dir, "src", k, tagDir(ref)+".tar.gz"); fileExists(p) {
			return p
		}
	}
	return ""
}

// serveRaw serves a file the way raw.githubusercontent.com would.
func (e *Emulator) serveRaw(w http.ResponseWriter, r *http.Request, owner, repo, ref, file string) {
	rp, key := e.repo(owner, repo)
	if rp == nil {
		http.Error(w, "404: Not Found (repo not mirrored)", http.StatusNotFound)
		return
	}
	if file == "" || !validTag(strings.ReplaceAll(ref, "/", "_")) {
		http.Error(w, "404: Not Found", http.StatusNotFound)
		return
	}
	// 1) individually stored raw file
	if p, err := safeJoin(repoDir(e.data, key), "raw", tagDir(ref), file); err == nil && fileExists(p) {
		http.ServeFile(w, r, p)
		return
	}
	// 2) from a source snapshot of that ref; 3) branch-like refs fall back to the default branch
	tb := e.findTarball(key, rp, ref)
	fallback := false
	if tb == "" && !tagLikeRe.MatchString(ref) && rp.release(ref) == nil {
		tb, fallback = e.findTarball(key, rp, "refs/heads/"+rp.DefaultBranch), true
	}
	if tb == "" {
		http.Error(w, "404: Not Found (ref "+ref+" not mirrored)", http.StatusNotFound)
		return
	}
	data, mod, err := readFromTarball(tb, file)
	if err != nil {
		http.Error(w, "404: Not Found", http.StatusNotFound)
		return
	}
	if fallback {
		w.Header().Set("X-Mirror-Fallback", rp.DefaultBranch)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, path.Base(file), mod, bytes.NewReader(data))
}

// readFromTarball finds <topdir>/<file> in a GitHub source tarball.
func readFromTarball(tb, file string) ([]byte, time.Time, error) {
	f, err := os.Open(tb)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, time.Time{}, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, time.Time{}, os.ErrNotExist
		}
		_, inner, ok := strings.Cut(h.Name, "/")
		if !ok || inner != file || h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 256<<20))
		return b, h.ModTime, err
	}
}

type apiAsset struct {
	ID                 int    `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	State              string `json:"state"`
	ContentType        string `json:"content_type"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest,omitempty"`
}

type apiRelease struct {
	ID          int        `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt time.Time  `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
	HTMLURL     string     `json:"html_url"`
	URL         string     `json:"url"`
	TarballURL  string     `json:"tarball_url"`
	Body        string     `json:"body"`
	Assets      []apiAsset `json:"assets"`
}

func releaseJSON(base string, rp *Repo, rel *Release, id int) apiRelease {
	tagE := url.PathEscape(rel.Tag)
	out := apiRelease{ID: id, TagName: rel.Tag, Name: rel.Name, Prerelease: rel.Pre,
		PublishedAt: rel.Published, CreatedAt: rel.Published,
		HTMLURL:    base + "/gh/web/" + rp.Name + "/releases/tag/" + tagE,
		URL:        base + "/gh/api/repos/" + rp.Name + "/releases/tags/" + tagE,
		TarballURL: base + "/gh/web/" + rp.Name + "/archive/refs/tags/" + tagE + ".tar.gz",
		Body:       "Mirrored by sahand-mirror", Assets: []apiAsset{}}
	if out.Name == "" {
		out.Name = rel.Tag
	}
	for i, a := range rel.Assets {
		dl := base + "/gh/web/" + rp.Name + "/releases/download/" + tagE + "/" + url.PathEscape(a.Name)
		as := apiAsset{ID: id*1000 + i + 1, Name: a.Name, Size: a.Size, State: "uploaded",
			ContentType: "application/octet-stream", URL: dl, BrowserDownloadURL: dl}
		if a.SHA256 != "" {
			as.Digest = "sha256:" + a.SHA256
		}
		out.Assets = append(out.Assets, as)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (e *Emulator) serveAPI(w http.ResponseWriter, r *http.Request, s []string) {
	if len(s) < 3 || s[0] != "repos" {
		notFound(w, "endpoint not emulated")
		return
	}
	rp, _ := e.repo(s[1], s[2])
	if rp == nil {
		notFound(w, s[1]+"/"+s[2]+" is not mirrored")
		return
	}
	base := baseURL(r)
	rest := s[3:]
	idx := func(rel *Release) int {
		for i := range rp.Releases {
			if &rp.Releases[i] == rel {
				return i + 1
			}
		}
		return 1
	}
	switch {
	case len(rest) == 0:
		writeJSON(w, 200, map[string]any{"name": path.Base(rp.Name), "full_name": rp.Name,
			"default_branch": rp.DefaultBranch, "html_url": base + "/gh/web/" + rp.Name, "private": false})
	case len(rest) == 1 && rest[0] == "releases":
		list := []apiRelease{}
		for i := range rp.Releases {
			list = append(list, releaseJSON(base, rp, &rp.Releases[i], i+1))
		}
		writeJSON(w, 200, list)
	case len(rest) == 2 && rest[0] == "releases" && rest[1] == "latest":
		rel := rp.latestRelease()
		if rel == nil {
			notFound(w, "no releases mirrored")
			return
		}
		writeJSON(w, 200, releaseJSON(base, rp, rel, idx(rel)))
	case len(rest) == 3 && rest[0] == "releases" && rest[1] == "tags":
		rel := rp.release(rest[2])
		if rel == nil {
			notFound(w, "release "+rest[2]+" not mirrored")
			return
		}
		writeJSON(w, 200, releaseJSON(base, rp, rel, idx(rel)))
	case len(rest) == 1 && rest[0] == "tags":
		list := []map[string]string{}
		for _, rel := range rp.Releases {
			list = append(list, map[string]string{"name": rel.Tag,
				"tarball_url": base + "/gh/web/" + rp.Name + "/archive/refs/tags/" + url.PathEscape(rel.Tag) + ".tar.gz"})
		}
		writeJSON(w, 200, list)
	case len(rest) >= 2 && rest[0] == "tarball":
		e.serveArchive(w, r, s[1], s[2], strings.Join(rest[1:], "/"))
	default:
		notFound(w, "endpoint not emulated")
	}
}

// ServeExt serves /ext?u=<original url>.
func (e *Emulator) ServeExt(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	var x *Ext
	e.store.View(func(st *State) {
		x = st.Ext[u]
		if x == nil {
			x = st.Ext[strings.TrimSuffix(u, "/")]
		}
		if x == nil && !strings.HasSuffix(u, "/") {
			x = st.Ext[u+"/"]
		}
	})
	if x == nil || !x.Mirror || x.Fetched.IsZero() {
		http.Error(w, "not mirrored: "+u, http.StatusNotFound)
		return
	}
	p := extPath(e.data, x.URL)
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "not mirrored: "+u, http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	if b := make([]byte, 2); true {
		f.Read(b)
		f.Seek(0, io.SeekStart)
		if string(b) == "#!" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
	}
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
