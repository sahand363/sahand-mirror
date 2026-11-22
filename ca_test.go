package main

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestCAIssue(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreateCA(dir) // reload from disk
	if err != nil || string(again.CertPEM) != string(ca.CertPEM) {
		t.Fatal("CA not persisted")
	}
	certPEM, _, err := again.Issue([]string{"10.0.0.12", "panel.example.ir"}, 365)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM)
	for _, host := range []string{"10.0.0.12", "panel.example.ir"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			t.Errorf("verify %s: %v", host, err)
		}
	}
	if _, _, err := ca.Issue([]string{"bad host!"}, 1); err == nil {
		t.Error("invalid host accepted")
	}
	if z, err := ca.IssueZip([]string{"10.0.0.1"}, 30); err != nil || len(z) < 100 {
		t.Error("zip failed")
	}
}
