package main

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	CertPEM []byte
	dir     string
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

// LoadOrCreateCA keeps the CA in <data>/ca (key 0600).
func LoadOrCreateCA(data string) (*CA, error) {
	dir := filepath.Join(data, "ca")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cp, kp := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if cb, err := os.ReadFile(cp); err == nil {
		kb, err := os.ReadFile(kp)
		if err != nil {
			return nil, err
		}
		cblk, _ := pem.Decode(cb)
		kblk, _ := pem.Decode(kb)
		if cblk == nil || kblk == nil {
			return nil, errors.New("corrupt CA files")
		}
		cert, err := x509.ParseCertificate(cblk.Bytes)
		if err != nil {
			return nil, err
		}
		key, err := x509.ParseECPrivateKey(kblk.Bytes)
		if err != nil {
			return nil, err
		}
		return &CA{cert, key, cb, dir}, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	tpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "sahand-mirror internal CA (" + host + ")", Organization: []string{"sahand-mirror"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	cpem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFileAtomic(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(cp, cpem, 0o644); err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{cert, key, cpem, dir}, nil
}

// Issue creates a server certificate for IPs and/or DNS names.
func (ca *CA) Issue(hosts []string, days int) (certPEM, keyPEM []byte, err error) {
	if days <= 0 || days > 3650 {
		days = 825
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, days),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else if validHost(h) {
			tpl.DNSNames = append(tpl.DNSNames, strings.ToLower(h))
		} else {
			return nil, nil, errors.New("invalid host: " + h)
		}
		if tpl.Subject.CommonName == "" {
			tpl.Subject.CommonName = h
		}
	}
	if tpl.Subject.CommonName == "" {
		return nil, nil, errors.New("no host given")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certPEM = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ca.CertPEM...) // full chain
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	return certPEM, keyPEM, nil
}

func validHost(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(strings.TrimPrefix(h, "*."), ".") {
		if l == "" || len(l) > 63 {
			return false
		}
		for _, c := range l {
			if !(c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				return false
			}
		}
	}
	return true
}

func (ca *CA) IssueZip(hosts []string, days int) ([]byte, error) {
	c, k, err := ca.Issue(hosts, days)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	readme := "fullchain.pem = certificate + CA, privkey.pem = key, ca.crt = install on clients.\n" +
		"Example (3x-ui / x-ui): set cert path to fullchain.pem and key path to privkey.pem.\n" +
		"Trust on Debian/Ubuntu: cp ca.crt /usr/local/share/ca-certificates/sahand-mirror.crt && update-ca-certificates\n"
	for name, data := range map[string][]byte{"fullchain.pem": c, "privkey.pem": k, "ca.crt": ca.CertPEM, "README.txt": []byte(readme)} {
		f, _ := zw.Create(name)
		f.Write(data)
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TLSConfig returns a server cert for the hub's own hosts, issued by the CA.
func (ca *CA) TLSConfig(hosts []string) (*tls.Config, error) {
	c, k, err := ca.Issue(hosts, 825)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}
