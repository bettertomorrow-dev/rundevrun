package state

import (
	"errors"
	"path/filepath"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

// ProxyRecord identifies a daemon without trusting a reusable PID.
type ProxyRecord struct {
	Identity     procid.Ref `json:"identity"`
	State        string     `json:"state"`
	ConfigPath   string     `json:"config_path"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
}

func (s *Store) ReadProxy() (ProxyRecord, error) {
	var record ProxyRecord
	if _, err := s.Read(); err != nil {
		return record, err
	}
	b, err := readFile(filepath.Join(s.Root, "proxy.json"))
	if err != nil {
		return record, err
	}
	if err = decode(b, &record); err != nil {
		return record, err
	}
	if record.Identity.PID <= 0 || record.Identity.Identity == "" || record.Identity.BootID == "" || record.Identity.SID <= 0 || record.Identity.PGID <= 0 || record.ConfigPath == "" {
		return ProxyRecord{}, errors.New("invalid proxy identity record")
	}
	return record, nil
}
func (s *Store) WriteProxy(record ProxyRecord) error {
	if err := s.ensure(); err != nil {
		return err
	}
	return s.write(filepath.Join(s.Root, "proxy.json"), record)
}
func (s *Store) ProxySocket() string { return filepath.Join(s.Root, "proxy.sock") }
