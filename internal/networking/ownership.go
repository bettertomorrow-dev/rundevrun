// Implements networking ownership.
package networking

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type ownedResources struct {
	DNS                 *DNSRecord `json:"dns,omitempty"`
	Zone                string     `json:"zone,omitempty"`
	ServeTarget         string     `json:"serve_target,omitempty"`
	TermsURL            string     `json:"terms_url,omitempty"`
	Pending             string     `json:"pending,omitempty"`
	PendingDNS          *DNSChange `json:"pending_dns,omitempty"`
	PendingServe        string     `json:"pending_serve,omitempty"`
	PendingServeTarget  string     `json:"pending_serve_target,omitempty"`
	PendingChallenge    *DNSRecord `json:"pending_challenge,omitempty"`
	PendingTerms        string     `json:"pending_terms,omitempty"`
	PendingFinalization bool       `json:"pending_finalization,omitempty"`
}

func (s Service) readOwnership() (ownedResources, error) {
	var owned ownedResources
	b, err := readFile(filepath.Join(s.Root, "networking-owned.json"))
	if errors.Is(err, os.ErrNotExist) {
		return owned, nil
	}
	if err != nil {
		return owned, err
	}
	if err = json.Unmarshal(b, &owned); err != nil {
		return owned, errors.New("invalid networking ownership record")
	}
	return owned, nil
}
func (s Service) writeOwnership(owned ownedResources) error {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(s.Root)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("configuration root must be private")
	}
	data, err := json.MarshalIndent(owned, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, "\n"...)
	return atomicWrite(filepath.Join(s.Root, "networking-owned.json"), data)
}
