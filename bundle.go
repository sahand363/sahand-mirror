package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

type manifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Format     int            `json:"format"`
	Created    time.Time      `json:"created"`
	HubVersion string         `json:"hub_version"`
	Apps       []string       `json:"apps"`
	Files      []manifestFile `json:"files"`
}

func selfArch() string {
	if runtime.GOARCH != "arm" {
		return runtime.GOARCH
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" {
				return "armv" + strings.TrimSuffix(s.Value, ",softfloat")
			}
		}
	}
	return "armv7"
}

// ExportBundle writes repos, external files, state, hub binaries and
// install.sh as a tar.gz; manifest.json (sha256 of every file) comes last.
func ExportBundle(w io.Writer, data string, st *State) (*Manifest, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	m := &Manifest{Format: 1, Created: time.Now().UTC(), HubVersion: version}
	for _, a := range st.Apps {
		m.Apps = append(m.Apps, a.ID)
	}
	add := func(name string, r io.Reader, size int64, mode int64, mt time.Time) error {
		h := sha256.New()
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: size, Mode: mode, ModTime: mt, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		n, err := io.Copy(tw, io.TeeReader(io.LimitReader(r, size), h))
		if err != nil {
			return err
		}
		if n != size {
			return fmt.Errorf("%s changed while exporting", name)
		}
		m.Files = append(m.Files, manifestFile{name, size, hex.EncodeToString(h.Sum(nil))})
		return nil
	}
	addFile := func(name, p string, mode int64) error {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		return add(name, f, fi.Size(), mode, fi.ModTime())
	}
	st.Sync.Running, st.Sync.Progress = false, ""
	if st.Apt != nil {
		st.Apt.Running, st.Apt.Progress = false, ""
	}
	sb, _ := json.MarshalIndent(st, "", " ")
	if err := add("state.json", bytes.NewReader(sb), int64(len(sb)), 0o644, time.Now()); err != nil {
		return nil, err
	}
	for _, sub := range []string{"repos", "ext", "scripts", "apt"} {
		root := filepath.Join(data, sub)
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".tmp-") {
				return nil
			}
			rel, _ := filepath.Rel(data, p)
			return addFile(filepath.ToSlash(rel), p, 0o644)
		})
		if err != nil {
			return nil, err
		}
	}
	// hub binaries: ones imported earlier + the running one
	me := "sahand-mirror-linux-" + selfArch()
	ents, _ := os.ReadDir(filepath.Join(data, "self"))
	for _, e := range ents {
		if e.Type().IsRegular() && e.Name() != me && strings.HasPrefix(e.Name(), "sahand-mirror-linux-") {
			if err := addFile("self/"+e.Name(), filepath.Join(data, "self", e.Name()), 0o755); err != nil {
				return nil, err
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if err := addFile("self/"+me, exe, 0o755); err != nil {
			return nil, err
		}
	}
	if err := add("install.sh", bytes.NewReader(installScript), int64(len(installScript)), 0o755, time.Now()); err != nil {
		return nil, err
	}
	mb, _ := json.MarshalIndent(m, "", " ")
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Size: int64(len(mb)), Mode: 0o644, ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(mb); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return m, gz.Close()
}

func ExportBundleFile(out, data string, store *Store) (*Manifest, error) {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".tmp-bundle-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	os.Chmod(f.Name(), 0o644)
	m, err := ExportBundle(f, data, store.Snapshot())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return m, os.Rename(f.Name(), out)
}

var bundlePrefixes = []string{"repos/", "ext/", "self/", "scripts/", "apt/"}

// ImportBundle verifies every file against manifest.json, then merges it.
func ImportBundle(src string, data string, store *Store) (*Manifest, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not a tar.gz bundle: %w", err)
	}
	stage := filepath.Join(data, "tmp", "import-"+randHex(6))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	got := map[string]manifestFile{}
	var mbytes, sbytes []byte
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("corrupt bundle: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue // dirs, symlinks, devices are ignored
		}
		name := filepath.ToSlash(filepath.Clean(h.Name))
		switch {
		case name == "manifest.json":
			mbytes, _ = io.ReadAll(io.LimitReader(tr, 64<<20))
			continue
		case name == "state.json":
			sbytes, _ = io.ReadAll(io.LimitReader(tr, 64<<20))
			sum := sha256.Sum256(sbytes)
			got[name] = manifestFile{name, int64(len(sbytes)), hex.EncodeToString(sum[:])}
			continue
		case name == "install.sh":
			continue
		}
		ok := false
		for _, p := range bundlePrefixes {
			ok = ok || strings.HasPrefix(name, p)
		}
		if !ok {
			continue
		}
		dst, err := safeJoin(stage, name)
		if err != nil {
			return nil, fmt.Errorf("unsafe path in bundle: %q", h.Name)
		}
		n, sum, err := copyAtomic(dst, tr)
		if err != nil {
			return nil, err
		}
		got[name] = manifestFile{name, n, sum}
	}
	if mbytes == nil || sbytes == nil {
		return nil, errors.New("bundle has no manifest.json/state.json")
	}
	var m Manifest
	if err := json.Unmarshal(mbytes, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	listed := map[string]bool{}
	for _, mf := range m.Files {
		listed[mf.Path] = true
		if mf.Path == "install.sh" {
			continue
		}
		g, ok := got[mf.Path]
		if !ok {
			return nil, fmt.Errorf("missing file %s", mf.Path)
		}
		if g.SHA256 != mf.SHA256 || g.Size != mf.Size {
			return nil, fmt.Errorf("checksum mismatch: %s", mf.Path)
		}
	}
	for p := range got {
		if !listed[p] {
			return nil, fmt.Errorf("file not in manifest: %s", p)
		}
	}
	var bst State
	if err := json.Unmarshal(sbytes, &bst); err != nil {
		return nil, fmt.Errorf("state.json: %w", err)
	}
	// move verified files into place
	for p := range got {
		if p == "state.json" {
			continue
		}
		from, _ := safeJoin(stage, p)
		to, err := safeJoin(data, p)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(from, to); err != nil {
			return nil, err
		}
		if strings.HasPrefix(p, "self/") {
			os.Chmod(to, 0o755)
		}
	}
	err = store.Update(func(st *State) { mergeState(st, &bst) })
	return &m, err
}

// mergeState merges a bundle's state into ours: newer wins, local recipes kept.
func mergeState(st, b *State) {
	for _, ba := range b.Apps {
		la := st.App(ba.ID)
		switch {
		case la == nil:
			st.Apps = append(st.Apps, ba)
		case ba.LastSync.After(la.LastSync):
			recipe := la.Recipe
			*la = *ba
			if la.Recipe == "" {
				la.Recipe = recipe
			}
		}
	}
	for k, br := range b.Repos {
		if lr := st.Repos[k]; lr == nil || br.LastSync.After(lr.LastSync) {
			st.Repos[k] = br
		}
	}
	for u, be := range b.Ext {
		if le := st.Ext[u]; le == nil || be.Fetched.After(le.Fetched) {
			st.Ext[u] = be
		}
	}
	for k, v := range b.Aliases {
		if st.Aliases == nil {
			st.Aliases = map[string]string{}
		}
		st.Aliases[k] = v
	}
	if b.Apt != nil {
		if st.Apt == nil {
			st.Apt = &AptState{}
		}
		if st.Apt.Targets == nil {
			st.Apt.Targets = map[string]*AptTargetState{}
		}
		for k, bt := range b.Apt.Targets {
			if lt := st.Apt.Targets[k]; lt == nil || bt.At.After(lt.At) {
				st.Apt.Targets[k] = bt
			}
		}
	}
	st.Sync.LastImport = time.Now()
	if b.Sync.LastOK.After(st.Sync.LastOK) {
		st.Sync.LastOK = b.Sync.LastOK
	}
}
