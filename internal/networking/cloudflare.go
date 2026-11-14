package networking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// DNSRecord is the non-secret subset used for reconciliation.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

// Cloudflare performs bounded API requests. Token is never serialized.
type Cloudflare struct {
	BaseURL string
	Client  *http.Client
	Token   string
}

type cfResponse struct {
	Success    bool            `json:"success"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

func (c Cloudflare) call(ctx context.Context, method, path string, input, output any) error {
	base := c.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare request failed: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	var envelope cfResponse
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || json.Unmarshal(b, &envelope) != nil || !envelope.Success {
		// Cloudflare may echo input. Never copy the response body to logs or output.
		return fmt.Errorf("cloudflare API rejected request (HTTP %d); check token permissions and retry", resp.StatusCode)
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return errors.New("invalid Cloudflare API response")
		}
	}
	return nil
}

// ListRecords checks only the exact requested name and type.
func (c Cloudflare) ListRecords(ctx context.Context, zone, name, recordType string) ([]DNSRecord, error) {
	if zone == "" || name == "" {
		return nil, errors.New("zone and record name are required")
	}
	var records []DNSRecord
	for page := 1; page <= 100; page++ {
		path := "/zones/" + url.PathEscape(zone) + "/dns_records?name.exact=" + url.QueryEscape(name) + fmt.Sprintf("&per_page=100&page=%d", page)
		if recordType != "" {
			path += "&type=" + url.QueryEscape(recordType)
		}
		// Use an envelope here to reject incomplete pagination rather than infer ownership.
		base := c.BaseURL
		if base == "" {
			base = "https://api.cloudflare.com/client/v4"
		}
		client := c.Client
		if client == nil {
			client = &http.Client{Timeout: 15 * time.Second}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cloudflare request failed: %w", err)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var envelope cfResponse
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || json.Unmarshal(b, &envelope) != nil || !envelope.Success {
			return nil, fmt.Errorf("cloudflare API rejected request (HTTP %d); check token permissions and retry", resp.StatusCode)
		}
		var batch []DNSRecord
		if json.Unmarshal(envelope.Result, &batch) != nil {
			return nil, errors.New("invalid Cloudflare DNS response")
		}
		for _, record := range batch {
			if strings.EqualFold(strings.TrimSuffix(record.Name, "."), strings.TrimSuffix(name, ".")) && (recordType == "" || record.Type == recordType) {
				records = append(records, record)
			}
		}
		if envelope.ResultInfo.TotalPages <= page {
			return records, nil
		}
	}
	return nil, errors.New("Cloudflare DNS results exceed pagination limit")
}

// StoreCredential installs a user-supplied token via an in-memory secret. It is
// never placed in a command argument, plan, or returned value.
func (s Service) StoreCredential(ref string, token []byte) error {
	if !refName.MatchString(ref) || len(token) > 4096 || len(bytes.TrimSpace(token)) == 0 {
		return errors.New("invalid credential reference or empty token")
	}
	if bytes.ContainsAny(token, "\r\n") {
		return errors.New("token must be a single line")
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
	dir := filepath.Join(s.Root, "credentials")
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("credentials directory must be private")
	}
	f, err := os.OpenFile(filepath.Join(dir, ref), os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(token)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(filepath.Join(dir, ref))
	}
	return err
}

func (s Service) credential(ref string) (string, error) {
	if !refName.MatchString(ref) {
		return "", errors.New("invalid credential reference")
	}
	path := filepath.Join(s.Root, "credentials", ref)
	// Reject overly permissive files before using their contents.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return "", errors.New("credential must be a private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(b) == 0 || len(b) > 4096 || bytes.ContainsAny(b, "\r\n") {
		return "", errors.New("invalid credential file")
	}
	return string(b), nil
}

func sameDNS(a, b DNSRecord) bool {
	return a.ID == b.ID && strings.EqualFold(a.Name, b.Name) && a.Type == b.Type && a.Content == b.Content && a.Proxied == b.Proxied
}

// DNSChange describes a reviewed wildcard mutation and its observed baseline.
type DNSChange struct {
	Zone     string     `json:"zone"`
	Desired  DNSRecord  `json:"desired"`
	Previous *DNSRecord `json:"previous,omitempty"`
}

// PlanWildcard refuses unmanaged or externally modified records. The caller
// supplies the last recorded owned value; nil means no record was created by rdr.
func (c Cloudflare) PlanWildcard(ctx context.Context, zone, domain, ip string, owned *DNSRecord) (DNSChange, error) {
	address := net.ParseIP(ip)
	_, v4net, _ := net.ParseCIDR("100.64.0.0/10")
	_, v6net, _ := net.ParseCIDR("fd7a:115c:a1e0::/48")
	if address == nil || (!v4net.Contains(address) && !v6net.Contains(address)) {
		return DNSChange{}, errors.New("address is not a Tailscale IP")
	}
	check := DefaultConfig()
	check.Tailnet.Domain = domain
	if err := validate(check); err != nil {
		return DNSChange{}, err
	}
	kind := "A"
	if address.To4() == nil {
		kind = "AAAA"
	}
	desired := DNSRecord{Type: kind, Name: "*." + strings.TrimSuffix(strings.ToLower(domain), "."), Content: address.String()}
	if domain == "" || zone == "" {
		return DNSChange{}, errors.New("zone and domain are required")
	}
	records, err := c.ListRecords(ctx, zone, desired.Name, "")
	if err != nil {
		return DNSChange{}, err
	}
	if owned == nil {
		if len(records) > 0 {
			return DNSChange{}, errors.New("wildcard DNS record is unmanaged; review and explicitly adopt or remove it")
		}
		return DNSChange{Zone: zone, Desired: desired}, nil
	}
	if len(records) != 1 || !sameDNS(records[0], *owned) {
		return DNSChange{}, errors.New("owned wildcard DNS record changed externally; inspect it before retrying")
	}
	desired.ID = owned.ID
	old := *owned
	return DNSChange{Zone: zone, Desired: desired, Previous: &old}, nil
}

// ApplyWildcard rechecks the observed baseline immediately before mutation.
// If a create response is lost, callers must resolve ownership manually.
func (c Cloudflare) ApplyWildcard(ctx context.Context, change DNSChange) (DNSRecord, error) {
	name := strings.TrimPrefix(change.Desired.Name, "*.")
	next, err := c.PlanWildcard(ctx, change.Zone, name, change.Desired.Content, change.Previous)
	if err != nil {
		return DNSRecord{}, err
	}
	if !sameDNS(next.Desired, change.Desired) {
		return DNSRecord{}, ErrConflict
	}
	if change.Previous != nil && sameDNS(*change.Previous, change.Desired) {
		return *change.Previous, nil
	}
	path := "/zones/" + url.PathEscape(change.Zone) + "/dns_records"
	method := http.MethodPost
	if change.Previous != nil {
		path += "/" + url.PathEscape(change.Previous.ID)
		method = http.MethodPut
	}
	var result DNSRecord
	if err := c.call(ctx, method, path, change.Desired, &result); err != nil {
		return DNSRecord{}, fmt.Errorf("%w; mutation outcome may be unknown, inspect the exact record before retrying", err)
	}
	expected := change.Desired
	expected.ID = result.ID
	if result.ID == "" || !sameDNS(expected, result) {
		return DNSRecord{}, errors.New("Cloudflare returned an unexpected DNS record; inspect it before retrying")
	}
	return result, nil
}

// ReplaceCredential explicitly rotates an existing token. The replacement is
// written privately and atomically so a failed rotation leaves the old token.
func (s Service) ReplaceCredential(ref string, token []byte) error {
	if !refName.MatchString(ref) || len(token) == 0 || len(token) > 4096 || bytes.ContainsAny(token, "\r\n") || len(bytes.TrimSpace(token)) == 0 {
		return errors.New("invalid credential reference or empty token")
	}
	if _, err := s.credential(ref); err != nil {
		return err
	}
	dir := filepath.Join(s.Root, "credentials")
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("credentials directory must be private")
	}
	f, err := os.CreateTemp(dir, ".credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(token); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, ref)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
