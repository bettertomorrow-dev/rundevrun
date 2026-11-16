// Manages local TLS certificates and credentials.
package routing

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func tlsInvalid(message string) error { return &Error{Code: "TLS_INVALID", Err: errors.New(message)} }

func LoadTLS(c Config, hostnames []string) (*tls.Config, error) {
	if c.ExternalTLS() {
		return nil, tlsInvalid("external TLS does not provide a server certificate")
	}
	if err := c.validateTLSMode(); err != nil {
		return nil, err
	}
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

func ValidateHosts(c Config, hosts []string) error {
	if err := c.validateTLSMode(); err != nil {
		return err
	}
	if !c.ExternalTLS() {
		_, err := LoadTLS(c, hosts)
		return err
	}
	now := time.Now()
	var leaves []*x509.Certificate
	for i, pair := range c.Routing.TLS.Certificates {
		data, err := os.ReadFile(pair.CertFile)
		if err != nil {
			return tlsInvalid(fmt.Sprintf("cannot read certificate %d", i+1))
		}
		count := 0
		for len(strings.TrimSpace(string(data))) > 0 {
			data = []byte(strings.TrimSpace(string(data)))
			if !strings.HasPrefix(string(data), "-----BEGIN CERTIFICATE-----") {
				return tlsInvalid(fmt.Sprintf("invalid public certificate chain %d", i+1))
			}
			block, rest := pem.Decode(data)
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return tlsInvalid(fmt.Sprintf("invalid public certificate chain %d", i+1))
			}
			cert, e := x509.ParseCertificate(block.Bytes)
			if e != nil || now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				return tlsInvalid(fmt.Sprintf("certificate %d has an invalid date or format", i+1))
			}
			if count == 0 {
				leaves = append(leaves, cert)
			}
			count++
			data = rest
		}
		if count == 0 {
			return tlsInvalid(fmt.Sprintf("empty certificate chain %d", i+1))
		}
	}
	for _, host := range hosts {
		host = strings.ToLower(host)
		if _, err := validateDNS(host); err != nil {
			return tlsInvalid("invalid hostname")
		}
		covered := false
		for _, cert := range leaves {
			if cert.VerifyHostname(host) == nil {
				covered = true
				break
			}
		}
		if !covered {
			return tlsInvalid("no valid certificate covers hostname " + host)
		}
	}
	return nil
}
