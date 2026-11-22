package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// validName reports whether s is a safe single path element (owner, repo, asset, tag).
func validName(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= 255 && nameRe.MatchString(s)
}

// safeJoin joins untrusted relative parts under base and refuses anything
// that would escape it (.., absolute paths, NUL bytes).
func safeJoin(base string, parts ...string) (string, error) {
	rel := filepath.Join(parts...)
	if strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) {
		return "", errors.New("bad path")
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", errors.New("path traversal")
		}
	}
	full := filepath.Join(base, rel)
	b := filepath.Clean(base)
	if full != b && !strings.HasPrefix(full, b+string(os.PathSeparator)) {
		return "", errors.New("path traversal")
	}
	return full, nil
}

// writeFileAtomic writes via temp file + fsync + rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// copyAtomic streams r into path atomically, returning size and sha256.
func copyAtomic(path string, r io.Reader) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return 0, "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, "", err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), os.Rename(tmp, path)
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 14)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func dirSize(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func diskFree(dir string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// shQuote single-quotes s for bash.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
