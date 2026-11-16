// SetupPlan is a reviewable external operation. TermsAccepted is set only after the user has reviewed TermsURL.
package networking

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type SetupPlan struct {
	Baseline          string    `json:"baseline"`
	Domain            string    `json:"domain"`
	DNS               DNSChange `json:"dns"`
	DNSAction         string    `json:"dns_action"`
	ServeAction       string    `json:"serve_action"`
	ServeTarget       string    `json:"serve_target,omitempty"`
	CertificateAction string    `json:"certificate_action"`
	TermsURL          string    `json:"terms_url,omitempty"`
	TermsAccepted     bool      `json:"terms_accepted"`
}
type SetupResult struct {
	Steps  []string    `json:"steps"`
	Status SetupStatus `json:"status"`
}
type SetupStatus struct {
	Domain      string    `json:"domain,omitempty"`
	DNS         string    `json:"dns"`
	Certificate string    `json:"certificate"`
	Serve       string    `json:"serve"`
	Expiry      time.Time `json:"expiry,omitempty"`
	Diagnostics []string  `json:"diagnostics,omitempty"`
}

func (s Service) Check(ctx context.Context) (SetupStatus, error) {
	snap, err := s.Inspect(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	status := SetupStatus{Domain: snap.Config.Tailnet.Domain, DNS: "unknown", Serve: "unknown", Certificate: "unavailable"}
	owned, err := s.readOwnership()
	if err != nil {
		return status, err
	}
	if owned.DNS != nil {
		status.DNS = "configured; not checked remotely"
	}
	if owned.ServeTarget != "" {
		status.Serve = "configured; not checked remotely"
	}
	if owned.Pending != "" {
		status.Diagnostics = append(status.Diagnostics, "interrupted operation "+owned.Pending+"; inspect external resources before recovery")
	}
	if status.Domain != "" {
		cert, err := s.managedCertificate(status.Domain, s.issuer.roots, s.issuer.clock())
		if err == nil {
			status.Certificate = "valid"
			status.Expiry = cert.Leaf.NotAfter
		} else {
			status.Diagnostics = append(status.Diagnostics, "managed certificate unavailable: "+err.Error())
		}
	}
	return status, nil
}
func anyExposed(cfg Config) bool {
	for _, p := range cfg.Projects {
		if p.Tailnet {
			return true
		}
	}
	return false
}
func (s Service) cf(ref string) (Cloudflare, error) {
	token, err := s.credential(ref)
	if err != nil {
		return Cloudflare{}, fmt.Errorf("Cloudflare credential unavailable: %w", err)
	}
	c := s.cloudflare
	c.Token = token
	return c, nil
}

func (s Service) PlanSetup(ctx context.Context) (SetupPlan, error) {
	snap, err := s.Inspect(ctx)
	if err != nil {
		return SetupPlan{}, err
	}
	cfg := snap.Config.Tailnet
	plan := SetupPlan{Baseline: snap.Revision, Domain: cfg.Domain, DNSAction: "none", ServeAction: "none", CertificateAction: "none"}
	owned, err := s.readOwnership()
	if err != nil {
		return SetupPlan{}, err
	}
	plan.ServeTarget = ManagedTLSTarget(snap.Legacy)
	if owned.Pending != "" {
		return SetupPlan{}, fmt.Errorf("interrupted %s operation; inspect external resources and resolve networking-owned.json before retrying", owned.Pending)
	}
	if !anyExposed(snap.Config) && owned.ServeTarget == "" {
		return plan, nil
	}
	raw, err := s.tailscale.serve443(ctx)
	if err != nil {
		return SetupPlan{}, err
	}
	target, ok := rawForward(raw)
	if !anyExposed(snap.Config) {
		if owned.ServeTarget != "" {
			if !ok || target != owned.ServeTarget {
				return SetupPlan{}, errors.New("owned Serve forwarding was changed; inspect it before retrying")
			}
			plan.ServeAction = "remove"
			plan.ServeTarget = owned.ServeTarget
		}
		return plan, nil
	}
	if cfg.Domain == "" || cfg.ZoneID == "" || cfg.CredentialRef == "" || cfg.Email == "" {
		return SetupPlan{}, errors.New("tailnet domain, Cloudflare zone and token reference, and ACME email are required")
	}
	if owned.Zone != "" && owned.Zone != cfg.ZoneID {
		return SetupPlan{}, errors.New("previous DNS record belongs to a different zone; use explicit cleanup before changing zones")
	}
	if owned.DNS != nil && !strings.EqualFold(owned.DNS.Name, "*."+cfg.Domain) {
		return SetupPlan{}, errors.New("previous DNS record belongs to a different domain; use explicit cleanup before changing domains")
	}
	ip, err := s.tailscale.ip(ctx)
	if err != nil {
		return SetupPlan{}, err
	}
	cf, err := s.cf(cfg.CredentialRef)
	if err != nil {
		return SetupPlan{}, err
	}
	plan.DNS, err = cf.PlanWildcard(ctx, cfg.ZoneID, cfg.Domain, ip, owned.DNS)
	if err != nil {
		return SetupPlan{}, err
	}
	switch {
	case owned.DNS == nil:
		plan.DNSAction = "create"
	case sameDNS(*owned.DNS, plan.DNS.Desired):
		plan.DNSAction = "none"
	default:
		plan.DNSAction = "update"
	}
	if owned.ServeTarget == "" {
		if len(raw) != 0 {
			return SetupPlan{}, errors.New("tailnet port 443 has an unmanaged listener")
		}
		plan.ServeAction = "add"
	} else {
		if !ok || target != owned.ServeTarget || target != plan.ServeTarget {
			return SetupPlan{}, errors.New("owned Serve forwarding was changed; inspect it before retrying")
		}
		plan.ServeAction = "unchanged"
	}
	cert, err := s.managedCertificate(cfg.Domain, s.issuer.roots, s.issuer.clock())
	if err != nil || !cert.Leaf.NotAfter.After(s.issuer.clock().Add(30*24*time.Hour)) {
		plan.CertificateAction = "issue wildcard certificate"
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			return SetupPlan{}, keyErr
		}
		directory, err := s.issuer.client(key).Discover(ctx)
		if err != nil {
			return SetupPlan{}, fmt.Errorf("ACME directory: %w", err)
		}
		plan.TermsURL = directory.Terms
	}
	return plan, nil
}

func (s Service) setupLock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(s.Root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("configuration root must be private")
	}
	f, err := os.OpenFile(filepath.Join(s.Root, "networking-setup.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (s Service) checkBaseline(ctx context.Context, baseline string) error {
	snap, err := s.Inspect(ctx)
	if err != nil {
		return err
	}
	if snap.Revision != baseline {
		return ErrConflict
	}
	return nil
}
func equalPlans(a, b SetupPlan) bool {
	a.TermsAccepted = false
	b.TermsAccepted = false
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (s Service) ApplySetup(ctx context.Context, plan SetupPlan) (result SetupResult, resultErr error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return result, err
	}
	defer unlock()
	current, err := s.PlanSetup(ctx)
	if err != nil {
		return result, err
	}
	if !equalPlans(current, plan) {
		return result, ErrConflict
	}
	if plan.CertificateAction != "none" && plan.TermsURL != "" && !plan.TermsAccepted {
		return result, errors.New("review and accept the ACME terms before issuing a certificate")
	}
	snap, err := s.Inspect(ctx)
	if err != nil {
		return result, err
	}
	owned, err := s.readOwnership()
	if err != nil {
		return result, err
	}
	fail := func(stage string, e error) (SetupResult, error) {
		return result, fmt.Errorf("%s failed after %v: %w; inspect the owned resources and pending operation before retrying", stage, result.Steps, e)
	}
	stage := func(name string) error {
		if err := s.checkBaseline(ctx, plan.Baseline); err != nil {
			return err
		}
		owned.Pending = name
		return s.writeOwnership(owned)
	}
	done := func() error { clearPending(&owned); return s.writeOwnership(owned) }
	if plan.DNSAction != "none" {
		owned.PendingDNS = &plan.DNS
		if err = stage("dns"); err != nil {
			return result, err
		}
		cf, e := s.cf(snap.Config.Tailnet.CredentialRef)
		if e != nil {
			return fail("dns", e)
		}
		record, e := cf.ApplyWildcard(ctx, plan.DNS)
		if e != nil {
			return fail("dns", e)
		}
		owned.DNS = &record
		owned.Zone = plan.DNS.Zone
		if e = done(); e != nil {
			return fail("record DNS ownership", e)
		}
		result.Steps = append(result.Steps, "DNS "+plan.DNSAction)
	}
	if plan.CertificateAction != "none" {
		owned.PendingTerms = plan.TermsURL
		if err = stage("certificate"); err != nil {
			return result, err
		}
		cf, e := s.cf(snap.Config.Tailnet.CredentialRef)
		if e != nil {
			return fail("certificate", e)
		}
		issuer := s.issuer
		issuer.beforeChallenge = func(r DNSRecord) error { owned.PendingChallenge = &r; return s.writeOwnership(owned) }
		issuer.afterChallenge = func(r DNSRecord) error { owned.PendingChallenge = &r; return s.writeOwnership(owned) }
		issuer.afterCleanup = func() error { owned.PendingChallenge = nil; return s.writeOwnership(owned) }
		issuer.beforeFinalize = func() error { owned.PendingFinalization = true; return s.writeOwnership(owned) }
		if _, e = issuer.issueCertificate(ctx, s, cf, snap.Config.Tailnet, plan.TermsURL); e != nil {
			if owned.PendingChallenge == nil && !owned.PendingFinalization {
				if clearErr := done(); clearErr != nil {
					return fail("record certificate recovery", clearErr)
				}
				return result, fmt.Errorf("certificate issuance failed after cleanup; retry is safe: %w", e)
			}
			return fail("certificate", e)
		}
		owned.TermsURL = plan.TermsURL
		if e = done(); e != nil {
			return fail("record certificate ownership", e)
		}
		result.Steps = append(result.Steps, "certificate issued")
	}
	if plan.ServeAction == "add" || plan.ServeAction == "remove" {
		owned.PendingServe = plan.ServeAction
		owned.PendingServeTarget = plan.ServeTarget
		if err = stage("serve"); err != nil {
			return result, err
		}
		if e := s.tailscale.apply(ctx, plan.ServeAction, plan.ServeTarget); e != nil {
			return fail("serve", e)
		}
		if plan.ServeAction == "add" {
			owned.ServeTarget = plan.ServeTarget
		} else {
			owned.ServeTarget = ""
		}
		if e := done(); e != nil {
			return fail("record Serve ownership", e)
		}
		result.Steps = append(result.Steps, "Serve "+plan.ServeAction)
	}
	result.Status, err = s.Check(ctx)
	return result, err
}

func (s Service) RenewIfNeeded(ctx context.Context) (bool, error) {
	owned, err := s.readOwnership()
	if err != nil {
		return false, err
	}
	if owned.Pending != "" {
		return false, fmt.Errorf("networking %s operation requires recovery before renewal", owned.Pending)
	}
	if owned.ServeTarget == "" || owned.DNS == nil {
		return false, nil
	}
	snap, err := s.Inspect(ctx)
	if err != nil {
		return false, err
	}
	if !anyExposed(snap.Config) {
		return false, nil
	}
	cert, certErr := s.managedCertificate(snap.Config.Tailnet.Domain, s.issuer.roots, s.issuer.clock())
	if certErr == nil && cert.Leaf.NotAfter.After(s.issuer.clock().Add(30*24*time.Hour)) {
		return false, nil
	}
	plan, err := s.PlanSetup(ctx)
	if err != nil {
		return false, err
	}
	if plan.CertificateAction == "none" {
		return false, nil
	}
	if plan.DNSAction != "none" || plan.ServeAction != "unchanged" {
		return false, errors.New("renewal requires unchanged owned DNS and Serve forwarding; run setup")
	}
	if plan.TermsURL != owned.TermsURL {
		return false, errors.New("ACME terms changed; review them in setup before renewing")
	}
	plan.TermsAccepted = true
	_, err = s.ApplySetup(ctx, plan)
	if err != nil {
		return false, err
	}
	return true, nil
}
