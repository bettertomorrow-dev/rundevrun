// certificateIssuer has bounded seams for a staging server and DNS fixture. It is used only after explicit setup or by renewal of an existing setup.
package networking

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

const productionACME = "https://acme-v02.api.letsencrypt.org/directory"

type certificateIssuer struct {
	directory       string
	httpClient      *http.Client
	roots           *x509.CertPool
	resolveTXT      func(context.Context, string) ([]string, error)
	now             func() time.Time
	beforeChallenge func(DNSRecord) error
	afterChallenge  func(DNSRecord) error
	afterCleanup    func() error
	beforeFinalize  func() error
}

func (i certificateIssuer) clock() time.Time {
	if i.now != nil {
		return i.now()
	}
	return time.Now()
}

func (s Service) accountKey() (*ecdsa.PrivateKey, error) {
	dir := filepath.Join(s.Root, "certificates")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("certificate directory must be private")
	}
	path := filepath.Join(dir, "account-key.pem")
	data, err := readFile(path)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, errors.New("invalid ACME account key")
		}
		return x509.ParseECPrivateKey(block.Bytes)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}); err != nil {
		os.Remove(path)
		return nil, err
	}
	if err = f.Sync(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return key, nil
}

func (i certificateIssuer) client(key *ecdsa.PrivateKey) *acme.Client {
	directory := i.directory
	if directory == "" {
		directory = productionACME
	}
	client := i.httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &acme.Client{Key: key, DirectoryURL: directory, HTTPClient: client}
}

func (i certificateIssuer) issueCertificate(ctx context.Context, s Service, cf Cloudflare, cfg Tailnet, approvedTerms string) (_ *tls.Certificate, resultErr error) {
	if cfg.Domain == "" || cfg.ZoneID == "" || cfg.Email == "" {
		return nil, errors.New("domain, zone and ACME email are required")
	}
	if i.resolveTXT == nil {
		var zone struct {
			NameServers []string `json:"name_servers"`
		}
		if err := cf.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(cfg.ZoneID), nil, &zone); err != nil {
			return nil, fmt.Errorf("discovering authoritative DNS servers: %w", err)
		}
		if len(zone.NameServers) == 0 {
			return nil, errors.New("Cloudflare zone has no authoritative DNS servers")
		}
		servers := make([]string, len(zone.NameServers))
		for n, host := range zone.NameServers {
			servers[n] = net.JoinHostPort(host, "53")
		}
		i.resolveTXT = func(ctx context.Context, name string) ([]string, error) {
			return lookupAuthoritativeTXT(ctx, name, servers)
		}
	}
	key, err := s.accountKey()
	if err != nil {
		return nil, err
	}
	client := i.client(key)
	directory, err := client.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("ACME directory: %w", err)
	}
	if directory.Terms != "" && directory.Terms != approvedTerms {
		return nil, errors.New("ACME terms require explicit approval; refresh setup preview")
	}
	_, err = client.Register(ctx, &acme.Account{Contact: []string{"mailto:" + cfg.Email}}, func(terms string) bool { return terms == approvedTerms && terms != "" })
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("ACME account: %w", err)
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs("*."+cfg.Domain))
	if err != nil {
		return nil, fmt.Errorf("ACME order: %w", err)
	}
	for _, authURL := range order.AuthzURLs {
		auth, err := client.GetAuthorization(ctx, authURL)
		if err != nil {
			return nil, err
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		var challenge *acme.Challenge
		for _, candidate := range auth.Challenges {
			if candidate.Type == "dns-01" {
				challenge = candidate
				break
			}
		}
		if challenge == nil {
			return nil, errors.New("ACME server did not offer DNS-01")
		}
		value, err := client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return nil, err
		}
		record := DNSRecord{Type: "TXT", Name: "_acme-challenge." + strings.TrimSuffix(cfg.Domain, "."), Content: value}
		path := "/zones/" + url.PathEscape(cfg.ZoneID) + "/dns_records"
		var created DNSRecord
		if i.beforeChallenge != nil {
			if err = i.beforeChallenge(record); err != nil {
				return nil, err
			}
		}
		if err = cf.call(ctx, http.MethodPost, path, record, &created); err != nil {
			return nil, fmt.Errorf("creating DNS challenge: %w; inspect challenge records before retrying", err)
		}
		if created.ID == "" || created.Type != record.Type || !strings.EqualFold(created.Name, record.Name) || created.Content != record.Content {
			return nil, errors.New("Cloudflare returned unexpected challenge; inspect challenge records before retrying")
		}
		cleanup := func() error {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			current, err := cf.ListRecords(cleanupCtx, cfg.ZoneID, record.Name, "TXT")
			if err != nil {
				return err
			}
			for _, r := range current {
				if r.ID == created.ID {
					if !sameDNS(r, created) {
						return errors.New("challenge record changed externally; inspect before cleanup")
					}
					return cf.call(cleanupCtx, http.MethodDelete, path+"/"+url.PathEscape(created.ID), nil, nil)
				}
			}
			return nil
		}
		if i.afterChallenge != nil {
			if err = i.afterChallenge(created); err != nil {
				cleanupErr := cleanup()
				if cleanupErr != nil {
					return nil, fmt.Errorf("recording challenge: %v; cleanup: %w", err, cleanupErr)
				}
				return nil, err
			}
		}
		err = i.waitTXT(ctx, record.Name, record.Content)
		if err == nil {
			_, err = client.Accept(ctx, challenge)
		}
		if err == nil {
			_, err = client.WaitAuthorization(ctx, authURL)
		}
		cleanErr := cleanup()
		if cleanErr == nil && i.afterCleanup != nil {
			cleanErr = i.afterCleanup()
		}
		if err != nil && cleanErr != nil {
			return nil, fmt.Errorf("challenge failed: %v; cleanup failed: %w", err, cleanErr)
		}
		if err != nil {
			return nil, err
		}
		if cleanErr != nil {
			return nil, fmt.Errorf("challenge cleanup: %w", cleanErr)
		}
	}
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, err
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"*." + cfg.Domain}}, certKey)
	if err != nil {
		return nil, err
	}
	if i.beforeFinalize != nil {
		if err = i.beforeFinalize(); err != nil {
			return nil, err
		}
	}
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("ACME issuance: %w", err)
	}
	var bundle []byte
	for _, der := range chain {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return nil, err
	}
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	if err = s.publishCertificate(cfg.Domain, bundle, i.roots, i.clock()); err != nil {
		return nil, err
	}
	return s.managedCertificate(cfg.Domain, i.roots, i.clock())
}

func (i certificateIssuer) waitTXT(ctx context.Context, name, value string) error {
	resolve := i.resolveTXT
	if resolve == nil {
		resolve = net.DefaultResolver.LookupTXT
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		values, err := resolve(ctx, name)
		if err == nil {
			for _, v := range values {
				if v == value {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("DNS challenge did not propagate: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
