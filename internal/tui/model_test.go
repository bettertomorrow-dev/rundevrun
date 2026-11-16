// Tests tui model behavior.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/monitor"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestApplyPrefersErrorsOnlyInitially(t *testing.T) {
	m := Model{}
	m.Apply(monitor.Snapshot{PreferErrors: true, Services: []monitor.Service{{ID: "bad", Error: "build failed"}, {ID: "live", State: "running"}}})
	if m.Tab != 1 {
		t.Fatal("expected error tab on first render")
	}
	m.ToggleTab()
	m.Apply(monitor.Snapshot{PreferErrors: true, Services: []monitor.Service{{ID: "bad", Error: "build failed"}, {ID: "live", State: "running"}}})
	if m.Tab != 0 {
		t.Fatal("refresh reset user's selected tab")
	}
}

func TestViewportKeepsSelectionVisibleAndFitsTerminal(t *testing.T) {
	services := make([]monitor.Service, 30)
	for i := range services {
		services[i] = monitor.Service{ID: fmt.Sprint(i), ProjectID: "p", ProjectName: "project", Name: fmt.Sprintf("service-%d", i), Worktree: "/worktree", State: "running"}
	}
	m := newApp(context.Background(), nil, "", io.Discard)
	m.Apply(monitor.Snapshot{Services: services})
	m.Selected = "29"
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if m.view.YOffset == 0 {
		t.Fatal("viewport did not follow selection")
	}
	text := ansi.Strip(m.View())
	if !strings.Contains(text, "> service-29") {
		t.Fatalf("selected row missing: %s", text)
	}
	if lipgloss.Height(text) > 10 {
		t.Fatalf("view exceeds terminal height: %s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if lipgloss.Width(line) > 40 {
			t.Fatalf("line exceeds terminal width: %q", line)
		}
	}
}

func TestBubbleTeaNavigationAndLogViewport(t *testing.T) {
	m := newApp(context.Background(), nil, "", io.Discard)
	m.Apply(monitor.Snapshot{Services: []monitor.Service{{ID: "one", State: "running"}, {ID: "two", State: "running"}}})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.Selected != "two" {
		t.Fatalf("arrow did not select next service: %s", m.Selected)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.Tab != 1 {
		t.Fatal("tab did not select errors")
	}
	m.ShowLogs = true
	m.Logs = strings.Repeat("line\n", 50)
	m.syncView()
	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if m.view.YOffset == 0 {
		t.Fatal("Bubbles log viewport did not scroll")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.ShowLogs {
		t.Fatal("Escape did not return from logs")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("Ctrl-C did not quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Ctrl-C did not use Bubble Tea shutdown")
	}
}

func TestScopeFiltersAndKeepsServiceIdentity(t *testing.T) {
	m := Model{AllProjects: false, Snapshot: monitor.Snapshot{CurrentProjectID: "p1", Services: []monitor.Service{
		{ID: "one", ProjectID: "p1", ProjectName: "alpha", Name: "web", Worktree: "/a", State: "running"},
		{ID: "two", ProjectID: "p1", ProjectName: "alpha", Name: "web", Worktree: "/b", State: "running"},
		{ID: "three", ProjectID: "p2", ProjectName: "beta", Name: "api", State: "running"},
	}}}
	m.Initialized = true
	m.SelectFirst()
	if len(m.rows()) != 2 {
		t.Fatalf("current scope rows=%d", len(m.rows()))
	}
	m.ToggleScope()
	if len(m.rows()) != 3 {
		t.Fatalf("all scope rows=%d", len(m.rows()))
	}
	m.Selected = "two"
	m.Move(1)
	if m.Selected == "two" {
		t.Fatal("selection did not move")
	}
}

func TestDrawEscapesTerminalControlAndShowsOrphanAction(t *testing.T) {
	m := Model{Snapshot: monitor.Snapshot{Services: []monitor.Service{{ID: "x", ProjectID: "p", ProjectName: "bad\x1b[2J", Name: "web", Worktree: "/deleted", State: "running", MissingWorktree: true}}}, Selected: "x", Initialized: true}
	ui := newApp(context.Background(), nil, "", io.Discard)
	ui.Model = m
	ui.syncView()
	text := ansi.Strip(ui.View())
	if strings.Contains(text, "bad\x1b[2J") {
		t.Fatal("project name contains terminal escape")
	}
	if !strings.Contains(text, "Stop process without worktree") {
		t.Fatal("missing explicit orphan stop label")
	}
}

func TestDrawGroupsProjectsByIdentity(t *testing.T) {
	m := Model{Snapshot: monitor.Snapshot{Services: []monitor.Service{
		{ID: "one", ProjectID: "p1", ProjectName: "same", Name: "web", State: "running"},
		{ID: "two", ProjectID: "p2", ProjectName: "same", Name: "api", State: "running"},
	}}, Initialized: true}
	ui := newApp(context.Background(), nil, "", io.Discard)
	ui.Model = m
	ui.syncView()
	text := ansi.Strip(ui.View())
	if strings.Count(text, "same  [") != 2 {
		t.Fatalf("projects with same name were merged: %s", text)
	}
}

func TestNetworkingPanelPreviewsAndAppliesSettingsAsynchronously(t *testing.T) {
	ui := newApp(context.Background(), nil, "", io.Discard)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	store, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	ui.store = store
	ui.network = networking.Service{Root: root}
	ui.Snapshot = monitor.Snapshot{Services: []monitor.Service{{ID: "p/w/web", ProjectID: "p", ProjectName: "demo", WorktreeID: "w", Name: "web", State: "running"}}}
	_, cmd := ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd == nil {
		t.Fatal("networking view did not start an async inspect")
	}
	msg := cmd()
	_, _ = ui.Update(msg)
	if !ui.showNetwork {
		t.Fatal("networking view did not open")
	}
	_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'+'}})
	_, cmd = ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd == nil {
		t.Fatal("preview did not run asynchronously")
	}
	msg = cmd()
	_, _ = ui.Update(msg)
	if ui.networkPlan == nil || len(ui.networkPlan.Changes) == 0 {
		t.Fatal("preview omitted the local port change")
	}
	if strings.Contains(ui.View(), "off switch") {
		t.Fatal("networking view offers a local off switch")
	}
	_, cmd = ui.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("apply did not run asynchronously")
	}
	msg = cmd()
	_, cmd = ui.Update(msg)
	if cmd != nil {
		_, _ = ui.Update(cmd())
	}
	snapshot, err := ui.network.Inspect(context.Background())
	if err != nil || snapshot.Config.Local.Listen != "127.0.0.1:8081" {
		t.Fatalf("saved config=%+v err=%v", snapshot.Config, err)
	}
}

func TestProjectSettingKeysPreservePreviewBaselineWithoutHTTPEndpoints(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	store, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	service := networking.Service{Root: root}
	snapshot, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Config.Tailnet.Domain = "tail.example.test"
	snapshot.Config.Projects = map[string]networking.Project{
		"p": {Tailnet: false, DefaultService: "old-service", PrimaryWorktree: "old-worktree"},
	}
	initial, err := service.Plan(context.Background(), snapshot.Config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = service.Apply(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	ui := newApp(context.Background(), store, root, io.Discard)
	ui.network = service
	ui.showNetwork = true
	ui.Model = Model{Snapshot: monitor.Snapshot{Services: []monitor.Service{{
		ID: "p/new-worktree/new-service", ProjectID: "p", ProjectName: "project",
		Name: "new-service", WorktreeID: "new-worktree", State: "running", Protocol: "tcp",
	}}}, Selected: "p/new-worktree/new-service", Initialized: true}
	_, _ = ui.Update(networkMsg{snapshot: snapshot})
	baseline := snapshot.Config.Projects["p"]
	if selected, ok := ui.SelectedService(); !ok || selected.Protocol == "http" || selected.LocalURL != "" || selected.TailnetURL != "" {
		t.Fatalf("fixture unexpectedly has an HTTP endpoint: %+v", selected)
	}
	for _, key := range []rune{'t', 's', 'w'} {
		_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
	}
	persisted, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Config.Projects["p"]; got != baseline {
		t.Fatalf("editing project settings changed the retained baseline: before=%+v after=%+v", baseline, got)
	}
	_, cmd := ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd == nil {
		t.Fatal("settings preview did not start")
	}
	_, _ = ui.Update(cmd())
	if ui.networkPlan == nil {
		t.Fatalf("settings preview missing: %s", ui.Status)
	}
	preview := ansi.Strip(ui.View())
	for _, want := range []string{`tailnet: "false" -> "true"`, `default_service: "old-service" -> "new-service"`, `primary_worktree: "old-worktree" -> "new-worktree"`} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q: %s", want, preview)
		}
	}
}

func TestNetworkingViewShowsCertificateActivationFailure(t *testing.T) {
	ui := newApp(context.Background(), nil, "", io.Discard)
	ui.showNetwork = true
	ui.proxyInfo = proxy.Info{CertificateActivationPending: true, RenewalError: "proxy reload failed"}
	ui.syncView()
	text := ansi.Strip(ui.View())
	if !strings.Contains(text, "Certificate activation: pending") || !strings.Contains(text, "Renewal / activation error: proxy reload failed") {
		t.Fatalf("network status hides pending certificate activation: %s", text)
	}
}

func TestNetworkCredentialCancelKeepsSecretOutOfUIAndDisk(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ui := newApp(context.Background(), nil, "", io.Discard)
	ui.network = networking.Service{Root: root}
	ui.showNetwork = true
	_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	secret := "test-token-value"
	for _, r := range secret {
		_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if strings.Contains(ansi.Strip(ui.View()), secret) {
		t.Fatal("credential was shown in the view")
	}
	if !strings.Contains(ansi.Strip(ui.View()), strings.Repeat("•", len(secret))) {
		t.Fatal("credential input is not visibly masked")
	}
	_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if ui.credentialInput != nil || ui.credentialEdit {
		t.Fatal("cancel retained credential input")
	}
	if _, err := os.Stat(filepath.Join(root, "credentials", "cloudflare-token")); !os.IsNotExist(err) {
		t.Fatalf("cancel wrote credential: %v", err)
	}
}

func TestNetworkCheckRunsAsyncAndHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ui := newApp(ctx, nil, "", io.Discard)
	ui.network = networking.Service{Root: t.TempDir()}
	started := time.Now()
	cmd := ui.checkNetwork()
	if time.Since(started) > 50*time.Millisecond {
		t.Fatal("starting network check blocked the UI")
	}
	if !ui.networkBusy {
		t.Fatal("network check did not report progress")
	}
	cancel()
	msg := cmd()
	if err, ok := msg.(setupMsg); !ok || err.err == nil {
		t.Fatalf("cancel result=%+v", msg)
	}
	_, _ = ui.Update(msg)
	if ui.networkBusy {
		t.Fatal("cancel left progress stuck")
	}
}

func TestNetworkDraftRejectsManualEditBeforePreview(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	service := networking.Service{Root: root}
	snapshot, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	ui := newApp(context.Background(), store, root, io.Discard)
	ui.network = service
	ui.showNetwork = true
	ui.networkSnapshot = snapshot
	draft := snapshot.Config
	draft.Local.Listen = "127.0.0.1:8081"
	ui.networkSnapshot.Config = draft
	manual := snapshot.Config
	manual.Tailnet.Email = "manual@example.test"
	manualPlan, err := service.PlanAtRevision(context.Background(), manual, snapshot.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Apply(context.Background(), manualPlan); err != nil {
		t.Fatal(err)
	}
	_, cmd := ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd == nil {
		t.Fatal("preview did not run")
	}
	msg := cmd()
	_, _ = ui.Update(msg)
	if ui.networkPlan != nil || !strings.Contains(ui.Status, "changed") {
		t.Fatalf("stale draft status=%q plan=%+v", ui.Status, ui.networkPlan)
	}
	actual, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if actual.Config.Tailnet.Email != "manual@example.test" || actual.Config.Local.Listen != snapshot.Config.Local.Listen {
		t.Fatalf("manual edit overwritten: %+v", actual.Config)
	}
}

func TestConditionalRollbackCannotOverwriteLaterManualEdit(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	service := networking.Service{Root: root}
	initial, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	applied := initial.Config
	applied.Local.Listen = "127.0.0.1:8081"
	plan, err := service.PlanAtRevision(context.Background(), applied, initial.Revision)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := service.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	manual := saved.Config
	manual.Tailnet.Email = "manual@example.test"
	manualPlan, err := service.PlanAtRevision(context.Background(), manual, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Apply(context.Background(), manualPlan); err != nil {
		t.Fatal(err)
	}
	if err = rollbackNetworkSettings(context.Background(), service, initial.Config, saved.Revision); !errors.Is(err, networking.ErrConflict) {
		t.Fatalf("rollback error=%v", err)
	}
	actual, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if actual.Config.Tailnet.Email != "manual@example.test" {
		t.Fatalf("rollback overwrote manual edit: %+v", actual.Config)
	}
}

func TestTermsCannotBeAcceptedUntilVisibleAndPanelCanScroll(t *testing.T) {
	ui := newApp(context.Background(), nil, "", io.Discard)
	ui.showNetwork = true
	ui.view.Height = 3
	services := make([]monitor.Service, 24)
	for i := range services {
		services[i] = monitor.Service{ID: fmt.Sprint(i), ProjectID: fmt.Sprint("p", i), ProjectName: "project", Name: "web", State: "running"}
	}
	ui.Snapshot = monitor.Snapshot{Services: services}
	ui.setupPlan = &networking.SetupPlan{TermsURL: "https://acme.test/terms"}
	ui.syncView()
	if ui.termsVisible() {
		t.Fatal("terms unexpectedly visible before scrolling")
	}
	_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if ui.setupPlan.TermsAccepted {
		t.Fatal("accepted hidden terms")
	}
	for range 16 {
		_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		if ui.termsVisible() {
			break
		}
	}
	if !ui.termsVisible() {
		t.Fatal("terms could not be scrolled into view")
	}
	_, _ = ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if !ui.setupPlan.TermsAccepted {
		t.Fatal("visible terms were not accepted")
	}
}

func TestRejectedRouteCandidateKeepsNetworkConfigAndRegistry(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	store, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	var projects []state.Project
	for _, parent := range []string{"one", "two"} {
		worktree := filepath.Join(root, parent, "main")
		if err := os.MkdirAll(worktree, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "dev.toml"), []byte("version = 1\n[project]\nname = 'shared'\n[services.web]\ncommand = 'sleep 60'\nprotocol = 'http'\nport = 'auto'\n"), 0600); err != nil {
			t.Fatal(err)
		}
		project, err := discovery.Discover(worktree)
		if err != nil {
			t.Fatal(err)
		}
		wt := project.Worktrees[0]
		id := project.ID + "/" + wt.ID + "/web"
		projects = append(projects, state.Project{ID: project.ID, Name: project.Name, Root: project.Root,
			Worktrees: []state.Worktree{{ID: wt.ID, Root: wt.Root, Branch: "main"}},
			Instances: map[string]state.Instance{id: {ID: id, Service: "web", WorktreeID: wt.ID, State: "stopped"}}})
	}
	if err := store.Update(func(reg *state.Registry) error {
		for _, project := range projects {
			if err := reg.SetProject(project.ID, project); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	registryBefore, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	ui := newApp(context.Background(), store, root, io.Discard)
	ui.network = networking.Service{Root: root}
	plan, err := ui.network.Plan(context.Background(), networking.Config{Version: networking.ConfigVersion, Local: networking.Local{Listen: "127.0.0.1:8081"}, Projects: map[string]networking.Project{}})
	if err != nil {
		t.Fatal(err)
	}
	msg := ui.applyNetworkSettings(plan, 0, nil)()
	if got, ok := msg.(networkMsg); !ok || got.err == nil {
		t.Fatalf("route collision was not rejected: %+v", msg)
	}
	snapshot, err := ui.network.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.Local.Listen != "127.0.0.1:8080" {
		t.Fatalf("rejected candidate changed settings: %+v", snapshot.Config.Local)
	}
	registryAfter, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(registryBefore) != string(registryAfter) {
		t.Fatal("rejected route candidate changed registry")
	}
}
