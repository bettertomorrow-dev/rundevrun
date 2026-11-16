// Tests runner hostname alias behavior.
package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestHostnameAliasResolvesNormalizedCollision(t *testing.T) {
	r, p, w := aliasFixture(t)
	path := filepath.Join(w.Root, "dev.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n[services.api-a]\ncommand='echo one'\nport='auto'\n[services.api_a]\ncommand='echo two'\nport='auto'\n")...)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = r.Store.Update(func(reg *state.Registry) error {
		p, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		for _, name := range []string{"api-a", "api_a"} {
			id := p.ID + "/w/" + name
			p.Instances[id] = state.Instance{ID: id, Service: name, WorktreeID: "w", State: "stopped"}
		}
		return reg.SetProject(p.ID, p)
	}); err != nil {
		t.Fatal(err)
	}
	one, _ := r.Routing.Hostname("api-a", "old", "demo", "dev.example.com")
	two, _ := r.Routing.Hostname("api_a", "old", "demo", "dev.example.com")
	if one != two {
		t.Fatalf("fixture does not collide: %s %s", one, two)
	}
	before, err := os.ReadFile(filepath.Join(r.Store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	selected := Selection{p, w, "api-a"}
	plan, err := r.PlanHostnameAlias(context.Background(), selected, "SHORT.DEV.EXAMPLE.COM")
	if err != nil {
		t.Fatal(err)
	}
	if plan.NewHostname != "short.dev.example.com" || plan.NewURL != "https://short.dev.example.com:8443" || plan.OldHostname != "" {
		t.Fatalf("plan: %+v", plan)
	}
	after, err := os.ReadFile(filepath.Join(r.Store.Root, "registry.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("plan wrote state")
	}
	if _, err = r.SetHostnameAlias(context.Background(), selected, "SHORT.DEV.EXAMPLE.COM"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.SetHostnameAlias(context.Background(), Selection{p, w, "api_a"}, "second.dev.example.com"); err != nil {
		t.Fatal(err)
	}
	reg, err := r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Instances["p/w/api-a"].Hostname != "short.dev.example.com" || saved.Instances["p/w/api_a"].Hostname != "second.dev.example.com" || saved.Instances["p/other/web"].Hostname != "web-other-demo.dev.example.com" || saved.Worktrees[0].Alias != "old" {
		t.Fatalf("wrong route changes: %+v", saved)
	}
}
func TestHostnameAliasLiveAndFailures(t *testing.T) {
	r, p, w := aliasFixture(t)
	selected := Selection{p, w, "api"}
	file := filepath.Join(r.Store.Root, "registry.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "bad_name.dev.example.com", "nested.extra.dev.example.com", "wrong.example.com", "web-other-demo.dev.example.com", strings.Repeat("a", 64) + ".dev.example.com"} {
		if _, err = r.SetHostnameAlias(context.Background(), selected, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
		after, e := os.ReadFile(file)
		if e != nil || string(before) != string(after) {
			t.Fatalf("bad hostname %q mutated registry", bad)
		}
	}
	if _, err = r.PlanHostnameAlias(context.Background(), Selection{p, w, "worker"}, "worker.dev.example.com"); err == nil {
		t.Fatal("accepted non-HTTP service")
	}
	lock, err := r.Store.LockInstances("p/w/api")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.SetHostnameAlias(context.Background(), selected, "short.dev.example.com")
	_ = lock.Close()
	var re *Error
	if !errors.As(err, &re) || re.Code != "BUSY" {
		t.Fatalf("busy lock: %v", err)
	}
	originalKey := r.Routing.Routing.TLS.Certificates[0].KeyFile
	r.Routing.Routing.TLS.Certificates[0].KeyFile = "/nonexistent/key"
	_, err = r.SetHostnameAlias(context.Background(), selected, "short.dev.example.com")
	if !errors.As(err, &re) || re.Code != "TLS_INVALID" {
		t.Fatalf("bad certificate: %v", err)
	}
	r.Routing.Routing.TLS.Certificates[0].KeyFile = originalKey
	after, err := os.ReadFile(file)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed operation wrote registry")
	}
	plan, err := r.SetHostnameAlias(context.Background(), selected, "short.dev.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.RestartRequired || plan.RunID != "active" || plan.OldHostname != "api-old-demo.dev.example.com" || plan.NewHostname != "short.dev.example.com" {
		t.Fatalf("live plan: %+v", plan)
	}
	manifest, err := r.Store.ReadManifest("active")
	if err != nil || manifest.Addresses["url"] != "https://api-old-demo.dev.example.com:8443" {
		t.Fatalf("active environment changed: %+v %v", manifest, err)
	}
	reg, err := r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	project, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if project.Instances["p/w/api"].RunID != "active" || project.Instances["p/w/api"].Hostname != "short.dev.example.com" || project.Instances["p/w/web"].Hostname != "web-old-demo.dev.example.com" {
		t.Fatalf("incorrect alias update: %+v", project.Instances)
	}
}

func TestHostnameAliasBypassesOverlongDefault(t *testing.T) {
	r, p, w := aliasFixture(t)
	name := strings.Repeat("x", 60)
	path := filepath.Join(w.Root, "dev.toml")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("\n[services." + name + "]\ncommand='echo x'\nport='auto'\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	id := p.ID + "/" + w.ID + "/" + name
	if err = r.Store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		project.Instances[id] = state.Instance{ID: id, WorktreeID: w.ID, Service: name, State: "stopped"}
		return reg.SetProject(p.ID, project)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Routing.Hostname(name, "old", "demo", "dev.example.com"); err == nil {
		t.Fatal("fixture default unexpectedly fits")
	}
	plan, err := r.SetHostnameAlias(context.Background(), Selection{p, w, name}, "short.dev.example.com")
	if err != nil || plan.NewHostname != "short.dev.example.com" {
		t.Fatalf("explicit hostname: %+v %v", plan, err)
	}
}
