package networking

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigLifecycleAndLegacy(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "config")
	s := Service{Root: root}
	snap, err := s.Inspect(ctx)
	if err != nil || snap.Config.Local.Listen != "127.0.0.1:8080" || snap.Legacy != nil {
		t.Fatalf("initial: %+v %v", snap, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect created files: %v", err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("version=1\n[routing]\nenabled=true\ndomain='legacy.example'\n[routing.tls]\nmode='provided'\n[[routing.tls.certificates]]\ncert_file='cert.pem'\nkey_file='key.pem'\n")
	if err := os.WriteFile(filepath.Join(root, "config.toml"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	desired := snap.Config
	desired.Tailnet = Tailnet{Domain: "dev.example.com", ZoneID: "zone-id", CredentialRef: "cloudflare-token"}
	desired.Projects["clone-1"] = Project{Tailnet: true, DefaultService: "web", PrimaryWorktree: "main-id"}
	plan, err := s.Plan(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if out.Legacy == nil || !out.Legacy.Routing.Enabled || out.Legacy.Routing.Domain != "legacy.example" || out.Config.Projects["clone-1"].DefaultService != "web" {
		t.Fatalf("roundtrip: %+v", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil || string(got) != string(legacy) {
		t.Fatal("legacy config changed")
	}
	if _, err := s.Apply(ctx, plan); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale plan: %v", err)
	}
	mode, err := os.Stat(s.path())
	if err != nil || mode.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", mode, err)
	}
}

func TestConfigRejectsUnsafeSettings(t *testing.T) {
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	ctx := context.Background()
	for _, change := range []func(*Config){
		func(c *Config) { c.Local.Listen = "0.0.0.0:8080" },
		func(c *Config) { c.Local.Listen = "127.0.0.1:80" },
		func(c *Config) { c.Tailnet.CredentialRef = "../secret" },
		func(c *Config) { c.Tailnet.Domain = "bad domain" },
	} {
		c := DefaultConfig()
		change(&c)
		if _, err := s.Plan(ctx, c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestStaleDraftAndRollbackNeverOverwriteManualEdit(t *testing.T) {
	ctx := context.Background()
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	original, err := s.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := original.Config
	stale.Local.Listen = "127.0.0.1:8082"
	manual := original.Config
	manual.Tailnet.Domain = "manual.example.com"
	p, err := s.PlanAtRevision(ctx, manual, original.Revision)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.Apply(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlanAtRevision(ctx, stale, original.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale preview accepted: %v", err)
	}
	// A second writer changes the file after the rollback caller obtained its
	// expected revision. The write lock must reject the exact old revision.
	second := changed.Config
	second.Local.Listen = "127.0.0.1:9090"
	p, err = s.PlanAtRevision(ctx, second, changed.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, Plan{Baseline: changed.Revision, Desired: original.Config}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale rollback accepted: %v", err)
	}
	latest, err := s.Inspect(ctx)
	if err != nil || latest.Config.Local.Listen != "127.0.0.1:9090" || latest.Config.Tailnet.Domain != "manual.example.com" {
		t.Fatalf("manual edit overwritten: %+v %v", latest.Config, err)
	}
}

func TestPlanListsConcreteChangesInStableOrder(t *testing.T) {
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	ctx := context.Background()
	c := DefaultConfig()
	c.Local.Listen = "127.0.0.1:9090"
	c.Tailnet.Domain = "dev.example.com"
	c.Tailnet.CredentialRef = "token-ref"
	c.Projects["z"] = Project{Tailnet: true, DefaultService: "api"}
	c.Projects["a"] = Project{PrimaryWorktree: "main"}
	p, err := s.Plan(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(p.Changes, "\n")
	for _, want := range []string{`local.listen: "127.0.0.1:8080" -> "127.0.0.1:9090"`, `tailnet.domain: "" -> "dev.example.com"`, `tailnet.credential_ref: "" -> "token-ref"`, `projects["a"].primary_worktree: "" -> "main"`, `projects["z"].default_service: "" -> "api"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
	if strings.Index(joined, `projects["a"]`) > strings.Index(joined, `projects["z"]`) {
		t.Fatalf("unstable project order: %s", joined)
	}
}
