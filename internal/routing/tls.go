package routing

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func tlsInvalid(message string) error { return &Error{Code: "TLS_INVALID", Err: errors.New(message)} }

// LoadTLS validates every pair and planned hostname before returning a candidate
// configuration. The caller can atomically install it only after this succeeds.
func LoadTLS(c Config, hostnames []string) (*tls.Config, error) {
	if len(c.Routing.TLS.Certificates) == 0 {
		return nil, tlsInvalid("no certificates configured")
	}
	certs := make([]tls.Certificate, 0, len(c.Routing.TLS.Certificates))
	now := time.Now()
	for i, pair := range c.Routing.TLS.Certificates {
		certPEM, err := os.ReadFile(pair.CertFile)
		if err != nil {
			return nil, tlsInvalid(fmt.Sprintf("cannot read certificate %d", i+1))
		}
		keyPEM, err := os.ReadFile(pair.KeyFile)
		if err != nil {
			return nil, tlsInvalid(fmt.Sprintf("cannot read private key %d", i+1))
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, tlsInvalid(fmt.Sprintf("invalid certificate or private key in pair %d", i+1))
		}
		for _, der := range cert.Certificate {
			parsed, e := x509.ParseCertificate(der)
			if e != nil || now.Before(parsed.NotBefore) || now.After(parsed.NotAfter) {
				return nil, tlsInvalid(fmt.Sprintf("certificate %d has an invalid date or format", i+1))
			}
		}
		certs = append(certs, cert)
	}
	selectCert := func(host string, at time.Time) (*tls.Certificate, error) {
		host = strings.ToLower(host)
		if _, err := validateDNS(host); err != nil {
			return nil, tlsInvalid("invalid SNI hostname")
		}
		for i := range certs {
			cert := &certs[i]
			if cert.Leaf != nil && !at.Before(cert.Leaf.NotBefore) && !at.After(cert.Leaf.NotAfter) && cert.Leaf.VerifyHostname(host) == nil {
				return cert, nil
			}
		}
		return nil, tlsInvalid("no valid certificate covers hostname " + host)
	}
	for _, host := range hostnames {
		if _, err := selectCert(host, now); err != nil {
			return nil, err
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return selectCert(hello.ServerName, time.Now())
	}}, nil
}
