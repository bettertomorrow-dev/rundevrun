package networking

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ManagedCertificate returns a validated certificate loaded from the atomic
// bundle for this machine. A missing, expired or untrusted certificate does not
// affect local HTTP routing.
func (s Service) ManagedCertificate(domain string) (*tls.Certificate, error) {
	return s.managedCertificate(domain, nil, time.Now())
}

func (s Service) managedCertificate(domain string, roots *x509.CertPool, now time.Time) (*tls.Certificate, error) {
	if domain == "" || strings.ContainsAny(domain, "*/ ") {
		return nil, errors.New("invalid managed certificate domain")
	}
	path := filepath.Join(s.Root, "certificates", "current.pem")
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("managed certificate bundle must be a private regular file")
	}
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return validateCertificate(data, domain, roots, now)
}

func validateCertificate(bundle []byte, domain string, roots *x509.CertPool, now time.Time) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		return nil, fmt.Errorf("invalid managed certificate and key: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("managed certificate is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	wildcard := false
	for _, name := range leaf.DNSNames {
		if strings.EqualFold(name, "*."+domain) {
			wildcard = true
			break
		}
	}
	if !wildcard {
		return nil, errors.New("managed certificate does not contain the required wildcard")
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		intermediates.AddCert(cert)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{DNSName: "rdr-certificate-check." + domain, Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, fmt.Errorf("managed certificate failed verification: %w", err)
	}
	pair.Leaf = leaf
	return &pair, nil
}

// publishCertificate validates the complete replacement before atomically
// swapping it. Callers serialize issuance under the networking operation lock.
func (s Service) publishCertificate(domain string, bundle []byte, roots *x509.CertPool, now time.Time) error {
	if _, err := validateCertificate(bundle, domain, roots, now); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	root, err := os.Lstat(s.Root)
	if err != nil {
		return err
	}
	if !root.IsDir() || root.Mode().Perm()&0077 != 0 {
		return errors.New("configuration root must be private")
	}
	dir := filepath.Join(s.Root, "certificates")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("certificate directory must be private")
	}
	f, err := os.CreateTemp(dir, ".certificate-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(bundle); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "current.pem")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
