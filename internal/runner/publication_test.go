package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestClosePublicationNeverClaimedAndMissingActive(t *testing.T) {
	s, e := state.New(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	owner, e := procid.Observe(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	p := state.Publication{ID: "pending", ProjectID: "p", RootID: "p/w/s", RootRunID: "run", Mode: "live", Phase: "preparing", Cloudflared: "/bin/true", Owner: owner, Salt: make([]byte, 16), Verifier: make([]byte, 32), SigningKey: make([]byte, 32)}
	if e = s.WritePublications(state.PublicationRegistry{Version: 1, Publications: map[string]state.Publication{p.ID: p}}); e != nil {
		t.Fatal(e)
	}
	if e = ClosePublication(context.Background(), s, p.ID); e != nil {
		t.Fatal(e)
	}
	if e = ClosePublication(context.Background(), s, p.ID); e != nil {
		t.Fatal(e)
	}
	reg, e := s.ReadPublications()
	if e != nil || reg.Publications[p.ID].Phase != "closed" {
		t.Fatalf("not revoked: %+v %v", reg, e)
	}
	p.ID = "active"
	p.Phase = "active"
	reg.Publications[p.ID] = p
	if e = s.WritePublications(reg); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(s.Root, "publications.json"))
	if e != nil {
		t.Fatal(e)
	}
	e = ClosePublication(context.Background(), s, p.ID)
	var unknown *Error
	if !errors.As(e, &unknown) || unknown.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("expected unknown ownership: %v", e)
	}
	after, e := os.ReadFile(filepath.Join(s.Root, "publications.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("missing active runtime modified registry")
	}
}
