// Tests networking certificate behavior.
package networking

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"testing"
	"time"
)

func TestCertificateReplacementPreservesValidBundle(t *testing.T) {
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"*.dev.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(12 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	if err = s.publishCertificate("dev.example.com", bundle, roots, now); err != nil {
		t.Fatal(err)
	}
	if err = s.publishCertificate("wrong.example.com", bundle, roots, now); err == nil {
		t.Fatal("accepted wrong domain")
	}
	if _, err = s.managedCertificate("dev.example.com", roots, now); err != nil {
		t.Fatalf("lost old cert: %v", err)
	}
	if _, err = s.managedCertificate("dev.example.com", roots, now.Add(24*time.Hour)); err == nil {
		t.Fatal("accepted expired cert")
	}
}
