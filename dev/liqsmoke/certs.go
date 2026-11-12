package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// writeCerts writes a throwaway CA (ca.pem) and a certificate for host
// signed by it (cert.pem, key.pem) into dir, for the TLS proxy of the test
// stack. Nothing else trusts them.
//
// The directory is bind-mounted into the TLS proxy, whose unprivileged user
// (uid 101) differs from the host user. Docker Desktop on macOS ignores host
// modes on bind mounts; Linux (CI) does not, so the directory must be 0755
// and the files 0644, the key included: a throwaway test key.
func writeCerts(dir, host string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: read by another uid in a container, test certificates only
		return err
	}
	// MkdirAll keeps the mode of an existing directory (an earlier 0750 run).
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: as above
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "liqsmoke test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	files := []struct {
		name, typ string
		der       []byte
		mode      os.FileMode
	}{
		{"ca.pem", "CERTIFICATE", caDER, 0o644},
		{"cert.pem", "CERTIFICATE", der, 0o644},
		// Read by the proxy's unprivileged user in another container.
		{"key.pem", "EC PRIVATE KEY", keyDER, 0o644},
	}
	for _, f := range files {
		b := pem.EncodeToMemory(&pem.Block{Type: f.typ, Bytes: f.der})
		if err := os.WriteFile(filepath.Join(dir, f.name), b, f.mode); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}
