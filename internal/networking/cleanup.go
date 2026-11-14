package networking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// CleanupPlan previews release of rdr-owned public DNS and tailnet forwarding.
// Credentials, certificates and other provider records are retained.
type CleanupPlan struct {
	Baseline    string     `json:"baseline"`
	DNS         *DNSRecord `json:"dns,omitempty"`
	Zone        string     `json:"zone,omitempty"`
	ServeTarget string     `json:"serve_target,omitempty"`
	Actions     []string   `json:"actions"`
}

func (s Service) PlanCleanup(ctx context.Context) (CleanupPlan, error) {
	snap, err := s.Inspect(ctx)
	if err != nil {
		return CleanupPlan{}, err
	}
	owned, err := s.readOwnership()
	if err != nil {
		return CleanupPlan{}, err
	}
	if owned.Pending != "" {
		return CleanupPlan{}, errors.New("recover the pending setup operation before cleanup")
	}
	plan := CleanupPlan{Baseline: snap.Revision, Zone: owned.Zone, ServeTarget: owned.ServeTarget, Actions: []string{}}
	if owned.DNS != nil {
		if owned.Zone == "" {
			return CleanupPlan{}, errors.New("owned DNS record has no recorded zone")
		}
		cf, err := s.cf(snap.Config.Tailnet.CredentialRef)
		if err != nil {
			return CleanupPlan{}, err
		}
		records, err := cf.ListRecords(ctx, owned.Zone, owned.DNS.Name, "")
		if err != nil {
			return CleanupPlan{}, err
		}
		found := false
		for _, r := range records {
			if r.ID == owned.DNS.ID {
				if !sameDNS(r, *owned.DNS) {
					return CleanupPlan{}, errors.New("owned DNS record was edited externally; inspect it before cleanup")
				}
				found = true
			}
		}
		if !found {
			return CleanupPlan{}, errors.New("owned DNS record is missing; inspect it before cleanup")
		}
		record := *owned.DNS
		plan.DNS = &record
		plan.Actions = append(plan.Actions, fmt.Sprintf("delete DNS %s %s in zone %s (record %s)", record.Type, record.Name, owned.Zone, record.ID))
	}
	if owned.ServeTarget != "" {
		raw, err := s.tailscale.serve443(ctx)
		if err != nil {
			return CleanupPlan{}, err
		}
		target, ok := rawForward(raw)
		if !ok || target != owned.ServeTarget {
			return CleanupPlan{}, errors.New("owned Serve forwarding changed externally; inspect it before cleanup")
		}
		plan.Actions = append(plan.Actions, "remove tailnet TCP 443 forwarding to "+owned.ServeTarget)
	}
	return plan, nil
}

// ApplyCleanup rechecks the complete preview and journals each individual delete.
func (s Service) ApplyCleanup(ctx context.Context, plan CleanupPlan) (SetupStatus, error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	defer unlock()
	actual, err := s.PlanCleanup(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	a, _ := json.Marshal(actual)
	b, _ := json.Marshal(plan)
	if string(a) != string(b) {
		return SetupStatus{}, ErrConflict
	}
	owned, err := s.readOwnership()
	if err != nil {
		return SetupStatus{}, err
	}
	if plan.ServeTarget != "" {
		if err = s.checkBaseline(ctx, plan.Baseline); err != nil {
			return SetupStatus{}, err
		}
		owned.Pending = "cleanup_serve"
		owned.PendingServe = "remove"
		owned.PendingServeTarget = plan.ServeTarget
		if err = s.writeOwnership(owned); err != nil {
			return SetupStatus{}, err
		}
		if err = s.tailscale.apply(ctx, "remove", plan.ServeTarget); err != nil {
			return SetupStatus{}, fmt.Errorf("Serve cleanup failed; run setup recovery: %w", err)
		}
		owned.ServeTarget = ""
		clearPending(&owned)
		if err = s.writeOwnership(owned); err != nil {
			return SetupStatus{}, err
		}
	}
	if plan.DNS != nil {
		if err = s.checkBaseline(ctx, plan.Baseline); err != nil {
			return SetupStatus{}, err
		}
		snap, err := s.Inspect(ctx)
		if err != nil {
			return SetupStatus{}, err
		}
		cf, err := s.cf(snap.Config.Tailnet.CredentialRef)
		if err != nil {
			return SetupStatus{}, err
		}
		// Recheck immediately before deletion. Never delete a replacement or a
		// manually edited record, even if its old ID is still present.
		records, err := cf.ListRecords(ctx, plan.Zone, plan.DNS.Name, "")
		if err != nil {
			return SetupStatus{}, err
		}
		found := false
		for _, r := range records {
			if r.ID == plan.DNS.ID {
				if !sameDNS(r, *plan.DNS) {
					return SetupStatus{}, errors.New("owned DNS record changed during cleanup")
				}
				found = true
			}
		}
		if !found {
			return SetupStatus{}, errors.New("owned DNS record disappeared during cleanup")
		}
		owned.Pending = "cleanup_dns"
		copyRecord := *plan.DNS
		owned.PendingDNS = &DNSChange{Zone: plan.Zone, Previous: &copyRecord, Desired: copyRecord}
		if err = s.writeOwnership(owned); err != nil {
			return SetupStatus{}, err
		}
		path := "/zones/" + url.PathEscape(plan.Zone) + "/dns_records/" + url.PathEscape(plan.DNS.ID)
		if err = cf.call(ctx, http.MethodDelete, path, nil, nil); err != nil {
			return SetupStatus{}, fmt.Errorf("DNS cleanup outcome is uncertain; run setup recovery: %w", err)
		}
		records, err = cf.ListRecords(ctx, plan.Zone, plan.DNS.Name, "")
		if err != nil {
			return SetupStatus{}, fmt.Errorf("DNS cleanup verification failed; run setup recovery: %w", err)
		}
		for _, r := range records {
			if r.ID == plan.DNS.ID {
				return SetupStatus{}, errors.New("owned DNS record remains after deletion; run setup recovery")
			}
		}
		owned.DNS = nil
		owned.Zone = ""
		clearPending(&owned)
		if err = s.writeOwnership(owned); err != nil {
			return SetupStatus{}, err
		}
	}
	return s.Check(ctx)
}
