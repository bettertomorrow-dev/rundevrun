package networking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

func (s Service) recoveryClient(ctx context.Context, owned ownedResources) (Cloudflare, error) {
	snap, err := s.Inspect(ctx)
	if err != nil {
		return Cloudflare{}, err
	}
	return s.cf(snap.Config.Tailnet.CredentialRef)
}
func clearPending(o *ownedResources) {
	o.Pending = ""
	o.PendingDNS = nil
	o.PendingServe = ""
	o.PendingServeTarget = ""
	o.PendingChallenge = nil
	o.PendingTerms = ""
	o.PendingFinalization = false
}

// RecoverSetup inspects interrupted operations without assuming ownership of
// ambiguous newly created records or listeners. Such cases need explicit
// adoption after the user inspects the resource.
func (s Service) RecoverSetup(ctx context.Context) (SetupStatus, error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	defer unlock()
	owned, err := s.readOwnership()
	if err != nil {
		return SetupStatus{}, err
	}
	switch owned.Pending {
	case "":
		return s.Check(ctx)
	case "dns":
		if owned.PendingDNS == nil {
			return SetupStatus{}, errors.New("pending DNS operation has no recorded intent")
		}
		change := owned.PendingDNS
		cf, err := s.recoveryClient(ctx, owned)
		if err != nil {
			return SetupStatus{}, err
		}
		records, err := cf.ListRecords(ctx, change.Zone, change.Desired.Name, "")
		if err != nil {
			return SetupStatus{}, err
		}
		if change.Previous == nil {
			if len(records) != 0 {
				return SetupStatus{}, fmt.Errorf("DNS creation outcome is ambiguous: inspect %s in zone %s and explicitly adopt its record ID if it matches the preview", change.Desired.Name, change.Zone)
			}
		} else {
			if len(records) != 1 {
				return SetupStatus{}, errors.New("owned DNS record no longer matches pending operation; inspect it before recovery")
			}
			expected := change.Desired
			expected.ID = change.Previous.ID
			if sameDNS(records[0], expected) {
				owned.DNS = &records[0]
				owned.Zone = change.Zone
			} else if !sameDNS(records[0], *change.Previous) {
				return SetupStatus{}, errors.New("owned DNS record was edited externally; inspect before recovery")
			}
		}
	case "serve":
		raw, err := s.tailscale.serve443(ctx)
		if err != nil {
			return SetupStatus{}, err
		}
		target, ok := rawForward(raw)
		switch owned.PendingServe {
		case "add":
			if len(raw) != 0 {
				if ok && target == owned.PendingServeTarget {
					return SetupStatus{}, errors.New("Serve add outcome is ambiguous; inspect port 443 and explicitly adopt the forwarding if it matches the preview")
				}
				return SetupStatus{}, errors.New("Serve port 443 belongs to another listener; inspect before recovery")
			}
		case "remove":
			if len(raw) == 0 {
				owned.ServeTarget = ""
			} else if !ok || target != owned.ServeTarget {
				return SetupStatus{}, errors.New("Serve forwarding changed externally; inspect before recovery")
			}
		default:
			return SetupStatus{}, errors.New("pending Serve operation has no recorded action")
		}
	case "cleanup_serve":
		raw, err := s.tailscale.serve443(ctx)
		if err != nil {
			return SetupStatus{}, err
		}
		target, ok := rawForward(raw)
		if len(raw) == 0 {
			owned.ServeTarget = ""
		} else if !ok || target != owned.PendingServeTarget {
			return SetupStatus{}, errors.New("Serve forwarding changed during cleanup; inspect before recovery")
		}
	case "cleanup_dns":
		if owned.PendingDNS == nil || owned.PendingDNS.Previous == nil {
			return SetupStatus{}, errors.New("pending DNS cleanup has no recorded record")
		}
		old := owned.PendingDNS.Previous
		cf, err := s.recoveryClient(ctx, owned)
		if err != nil {
			return SetupStatus{}, err
		}
		records, err := cf.ListRecords(ctx, owned.PendingDNS.Zone, old.Name, "")
		if err != nil {
			return SetupStatus{}, err
		}
		found := false
		for _, r := range records {
			if r.ID == old.ID {
				if !sameDNS(r, *old) {
					return SetupStatus{}, errors.New("owned DNS record changed during cleanup; inspect before recovery")
				}
				found = true
			}
		}
		if !found {
			owned.DNS = nil
			owned.Zone = ""
		}
	case "certificate":
		if owned.PendingFinalization {
			return SetupStatus{}, errors.New("certificate issuance outcome is ambiguous after finalization; inspect the ACME order before retrying")
		}
		if rec := owned.PendingChallenge; rec != nil {
			cf, err := s.recoveryClient(ctx, owned)
			if err != nil {
				return SetupStatus{}, err
			}
			if owned.Zone == "" {
				return SetupStatus{}, errors.New("challenge zone is missing from ownership record")
			}
			records, err := cf.ListRecords(ctx, owned.Zone, rec.Name, "TXT")
			if err != nil {
				return SetupStatus{}, err
			}
			for _, r := range records {
				if rec.ID == "" && r.Content == rec.Content {
					return SetupStatus{}, fmt.Errorf("challenge creation outcome is ambiguous for %s; inspect matching record %s and explicitly authorize its cleanup", rec.Name, r.ID)
				}
				if rec.ID != "" && r.ID == rec.ID {
					if !sameDNS(r, *rec) {
						return SetupStatus{}, errors.New("owned challenge changed externally; inspect before cleanup")
					}
					path := "/zones/" + url.PathEscape(owned.Zone) + "/dns_records/" + url.PathEscape(rec.ID)
					if err = cf.call(ctx, http.MethodDelete, path, nil, nil); err != nil {
						return SetupStatus{}, err
					}
				}
			}
		}
		snap, err := s.Inspect(ctx)
		if err != nil {
			return SetupStatus{}, err
		}
		if _, err = s.managedCertificate(snap.Config.Tailnet.Domain, s.issuer.roots, s.issuer.clock()); err == nil {
			owned.TermsURL = owned.PendingTerms
		}
	default:
		return SetupStatus{}, errors.New("unknown pending networking operation")
	}
	clearPending(&owned)
	if err = s.writeOwnership(owned); err != nil {
		return SetupStatus{}, err
	}
	return s.Check(ctx)
}

// AdoptPendingDNS explicitly claims one exact record after ambiguous creation.
func (s Service) AdoptPendingDNS(ctx context.Context, id string) (SetupStatus, error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	defer unlock()
	owned, err := s.readOwnership()
	if err != nil {
		return SetupStatus{}, err
	}
	if owned.Pending != "dns" || owned.PendingDNS == nil || owned.PendingDNS.Previous != nil || id == "" {
		return SetupStatus{}, errors.New("no ambiguous DNS creation to adopt")
	}
	change := owned.PendingDNS
	cf, err := s.recoveryClient(ctx, owned)
	if err != nil {
		return SetupStatus{}, err
	}
	records, err := cf.ListRecords(ctx, change.Zone, change.Desired.Name, "")
	if err != nil {
		return SetupStatus{}, err
	}
	expected := change.Desired
	expected.ID = id
	if len(records) != 1 || !sameDNS(records[0], expected) {
		return SetupStatus{}, errors.New("record ID or value differs from the pending DNS preview")
	}
	owned.DNS = &records[0]
	owned.Zone = change.Zone
	clearPending(&owned)
	if err = s.writeOwnership(owned); err != nil {
		return SetupStatus{}, err
	}
	return s.Check(ctx)
}

// AdoptPendingServe explicitly claims the exact pending raw forwarder.
func (s Service) AdoptPendingServe(ctx context.Context) (SetupStatus, error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	defer unlock()
	owned, err := s.readOwnership()
	if err != nil {
		return SetupStatus{}, err
	}
	if owned.Pending != "serve" || owned.PendingServe != "add" {
		return SetupStatus{}, errors.New("no ambiguous Serve addition to adopt")
	}
	raw, err := s.tailscale.serve443(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	target, ok := rawForward(raw)
	if !ok || target != owned.PendingServeTarget {
		return SetupStatus{}, errors.New("Serve listener differs from the pending preview")
	}
	owned.ServeTarget = target
	clearPending(&owned)
	if err = s.writeOwnership(owned); err != nil {
		return SetupStatus{}, err
	}
	return s.Check(ctx)
}

// ResolvePendingChallenge explicitly removes a matching challenge when the
// create response was lost before its ID could be stored.
func (s Service) ResolvePendingChallenge(ctx context.Context, id string) (SetupStatus, error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	defer unlock()
	owned, err := s.readOwnership()
	if err != nil {
		return SetupStatus{}, err
	}
	rec := owned.PendingChallenge
	if owned.Pending != "certificate" || rec == nil || rec.ID != "" || id == "" {
		return SetupStatus{}, errors.New("no ambiguous challenge creation to resolve")
	}
	cf, err := s.recoveryClient(ctx, owned)
	if err != nil {
		return SetupStatus{}, err
	}
	records, err := cf.ListRecords(ctx, owned.Zone, rec.Name, "TXT")
	if err != nil {
		return SetupStatus{}, err
	}
	expected := *rec
	expected.ID = id
	found := false
	for _, r := range records {
		if sameDNS(r, expected) {
			found = true
			break
		}
	}
	if !found {
		return SetupStatus{}, errors.New("challenge ID or value differs from recorded intent")
	}
	path := "/zones/" + url.PathEscape(owned.Zone) + "/dns_records/" + url.PathEscape(id)
	if err = cf.call(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return SetupStatus{}, err
	}
	clearPending(&owned)
	if err = s.writeOwnership(owned); err != nil {
		return SetupStatus{}, err
	}
	return s.Check(ctx)
}

// AdoptExistingDNS records explicit ownership of an existing wildcard only when
// its exact value matches the selected machine and the reviewed configuration.
func (s Service) AdoptExistingDNS(ctx context.Context, id string) (SetupStatus, error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	defer unlock()
	snap, err := s.Inspect(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	cfg := snap.Config.Tailnet
	if !anyExposed(snap.Config) || cfg.Domain == "" || cfg.ZoneID == "" || id == "" {
		return SetupStatus{}, errors.New("select exposed projects, domain, zone and record ID before adoption")
	}
	owned, err := s.readOwnership()
	if err != nil {
		return SetupStatus{}, err
	}
	if owned.Pending != "" || owned.DNS != nil {
		return SetupStatus{}, errors.New("resolve existing ownership or pending setup before adoption")
	}
	ip, err := s.tailscale.ip(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	cf, err := s.cf(cfg.CredentialRef)
	if err != nil {
		return SetupStatus{}, err
	}
	name := "*." + cfg.Domain
	records, err := cf.ListRecords(ctx, cfg.ZoneID, name, "")
	if err != nil {
		return SetupStatus{}, err
	}
	expected := DNSRecord{ID: id, Type: "A", Name: name, Content: ip, Proxied: false}
	if len(records) != 1 || !sameDNS(records[0], expected) {
		return SetupStatus{}, errors.New("DNS record must exactly match the reviewed DNS-only wildcard and local Tailscale IP")
	}
	owned.DNS = &records[0]
	owned.Zone = cfg.ZoneID
	if err = s.writeOwnership(owned); err != nil {
		return SetupStatus{}, err
	}
	return s.Check(ctx)
}

// ResolvePendingFinalization clears an ambiguous ACME finalization only after
// an explicit user decision. A CA may already have issued a certificate, so a
// subsequent order may count against issuer rate limits.
func (s Service) ResolvePendingFinalization(ctx context.Context) (SetupStatus, error) {
	unlock, err := s.setupLock(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	defer unlock()
	owned, err := s.readOwnership()
	if err != nil {
		return SetupStatus{}, err
	}
	if owned.Pending != "certificate" || !owned.PendingFinalization || owned.PendingChallenge != nil {
		return SetupStatus{}, errors.New("no ambiguous ACME finalization to resolve")
	}
	clearPending(&owned)
	if err = s.writeOwnership(owned); err != nil {
		return SetupStatus{}, err
	}
	return s.Check(ctx)
}
