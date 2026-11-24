package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppSizes(t *testing.T) {
	dir := t.TempDir()
	s := &Server{data: dir}
	p := filepath.Join(repoDir(dir, "a/b"), "releases", "v1", "x.tar.gz")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, make([]byte, 1000), 0o644)
	st := &State{Apps: []*App{{ID: "one", Repos: []string{"A/B", "a/b"}}, {ID: "two"}}}
	got := s.appSizes(st)
	if got["one"] != 1000 || got["two"] != 0 {
		t.Fatalf("sizes = %v", got)
	}
}

func TestPruneReleaseDir(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"x-linux-amd64.tar.gz", "x-windows-amd64.zip", ".tmp-1"} {
		os.WriteFile(filepath.Join(dir, n), []byte("1"), 0o644)
	}
	pruneReleaseDir(dir, []Asset{{Name: "x-linux-amd64.tar.gz"}})
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("left %d files", len(ents))
	}
}
