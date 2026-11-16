// Implements tui tui.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/monitor"
	"github.com/bettertomorrow-dev/rundevrun/internal/networkactivation"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

const help = "rdrtop shows rdr-managed services.\n\nUsage: rdrtop\n\n↑/k ↓/j move  Tab switches tabs  a toggles all projects\nn networking  l logs  o opens local URL  O opens tailnet URL\ns stops selected service  q quits\nLogs: ↑/k ↓/j or PgUp/PgDown scroll; q/Esc returns; Ctrl-C quits."

func Help() string { return help }

type tickMsg struct{}
type snapshotMsg struct {
	snapshot monitor.Snapshot
	err      error
}
type logsMsg struct {
	id, run, text string
	err           error
}
type actionMsg struct {
	stop bool
	err  error
}
type proxyReadyMsg struct{ err error }
type setupMsg struct {
	status                    networking.SetupStatus
	plan                      networking.SetupPlan
	result                    networking.SetupResult
	proxyInfo                 proxy.Info
	err                       error
	checked, planned, applied bool
	partial                   string
}
type networkMsg struct {
	snapshot      networking.Snapshot
	plan          networking.Plan
	routeChanges  []runner.LocalRouteChange
	routeRevision uint64
	err           error
	apply         bool
	planned       bool
}

type app struct {
	Model
	ctx                           context.Context
	store                         *state.Store
	cwd                           string
	renderer                      *lipgloss.Renderer
	view                          viewport.Model
	width, height                 int
	reading, logReading, stopping bool
	logService                    monitor.Service
	showNetwork, networkBusy      bool
	proxyEnsured, proxyEnsuring   bool
	networkEdit                   bool
	textField                     string
	credentialEdit                bool
	credentialInput               []byte
	network                       networking.Service
	networkSnapshot               networking.Snapshot
	networkPlan                   *networking.Plan
	setupStatus                   networking.SetupStatus
	proxyInfo                     proxy.Info
	setupPlan                     *networking.SetupPlan
	setupResult                   *networking.SetupResult
	setupPartial                  string
	routePreview                  []runner.LocalRouteChange
	routeRevision                 uint64
}

func newApp(ctx context.Context, store *state.Store, cwd string, out io.Writer) *app {
	network, _ := networking.DefaultService()
	return &app{ctx: ctx, store: store, cwd: cwd, network: network, renderer: lipgloss.NewRenderer(out), width: 100, height: 24, view: viewport.New(100, 19), reading: true}
}

func Run(ctx context.Context, store *state.Store, cwd string, in *os.File, out io.Writer) error {
	f, ok := out.(*os.File)
	if !ok || !term.IsTerminal(in.Fd()) || !term.IsTerminal(f.Fd()) {
		return fmt.Errorf("rdrtop requires an interactive terminal")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(newApp(ctx, store, cwd, out), tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out), tea.WithAltScreen()).Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (m *app) read() tea.Msg {
	s, err := monitor.Read(m.ctx, m.store, m.cwd)
	return snapshotMsg{s, err}
}
func (m *app) readLogs() tea.Cmd {
	s := m.logService
	m.logReading = true
	return func() tea.Msg {
		text, err := monitor.Logs(m.ctx, m.store, s, 100)
		return logsMsg{id: s.ID, run: s.RunID, text: text, err: err}
	}
}
func (m *app) inspectNetwork() tea.Cmd {
	m.networkBusy = true
	return func() tea.Msg {
		snapshot, err := m.network.Inspect(m.ctx)
		return networkMsg{snapshot: snapshot, err: err}
	}
}
func (m *app) checkNetwork() tea.Cmd {
	m.networkBusy = true
	return func() tea.Msg {
		status, err := m.network.Check(m.ctx)
		var info proxy.Info
		if m.store != nil {
			info, _ = proxy.Status(m.store)
		}
		return setupMsg{status: status, proxyInfo: info, err: err, checked: true}
	}
}
func (m *app) planSetup() tea.Cmd {
	m.networkBusy = true
	return func() tea.Msg {
		plan, err := m.network.PlanSetup(m.ctx)
		return setupMsg{plan: plan, err: err, planned: true}
	}
}
func (m *app) applySetup(plan networking.SetupPlan) tea.Cmd {
	m.networkBusy = true
	return func() tea.Msg {
		desired, err := m.network.Inspect(m.ctx)
		if err != nil {
			return setupMsg{err: err, applied: true}
		}
		path, err := routing.DefaultPath()
		if err != nil {
			return setupMsg{err: err, applied: true}
		}
		exe, err := os.Executable()
		if err != nil {
			return setupMsg{err: err, applied: true}
		}
		activation, err := networkactivation.Prepare(m.ctx, m.store, exe, path, desired.Config)
		if err != nil {
			return setupMsg{err: fmt.Errorf("setup preview rejected: %w", err), applied: true}
		}
		defer activation.Abort()
		result, err := m.network.ApplySetup(m.ctx, plan)
		if err != nil {
			return setupMsg{err: err, applied: true}
		}
		if err = activation.Commit(m.ctx); err != nil {
			return setupMsg{result: result, applied: true, partial: "Tailnet setup completed, but route commit failed: " + err.Error() + ". Run rdr network check --json and reconcile routes."}
		}
		if err = activation.Finish(m.ctx); err != nil {
			return setupMsg{result: result, applied: true, partial: "Tailnet setup completed, but TLS activation failed: " + err.Error() + ". Run rdr network setup-check --json, then rdr proxy reload."}
		}
		return setupMsg{result: result, applied: true}
	}
}
func (m *app) applyNetworkSettings(plan networking.Plan, expectedRouteRevision uint64, expectedChanges []runner.LocalRouteChange) tea.Cmd {
	m.networkBusy = true
	return func() tea.Msg {
		before, err := m.network.Inspect(m.ctx)
		if err != nil {
			return networkMsg{err: err}
		}
		configPath, err := routing.DefaultPath()
		if err != nil {
			return networkMsg{err: err}
		}
		exe, err := os.Executable()
		if err != nil {
			return networkMsg{err: err}
		}
		activation, err := networkactivation.Prepare(m.ctx, m.store, exe, configPath, plan.Desired)
		if err != nil {
			return networkMsg{err: fmt.Errorf("route preview rejected: %w", err)}
		}
		defer activation.Abort()
		if activation.RouteRevision() != expectedRouteRevision || !slices.Equal(activation.RouteChanges(), expectedChanges) {
			return networkMsg{err: errors.New("registered services or routes changed since preview; create a fresh preview")}
		}
		snapshot, err := m.network.Apply(m.ctx, plan)
		if err != nil {
			return networkMsg{err: err}
		}
		if err = activation.Commit(m.ctx); err != nil {
			rollbackErr := rollbackNetworkSettings(m.ctx, m.network, before.Config, snapshot.Revision)
			if rollbackErr == nil {
				return networkMsg{err: fmt.Errorf("route plan changed before it could be committed; settings were rolled back: %w", err)}
			}
			return networkMsg{snapshot: snapshot, err: fmt.Errorf("route commit failed after settings were saved: %v; settings rollback failed: %w", err, rollbackErr)}
		}
		if err = activation.Finish(m.ctx); err != nil {
			return networkMsg{snapshot: snapshot, err: fmt.Errorf("settings saved but proxy reload failed: %w", err)}
		}
		return networkMsg{snapshot: snapshot, apply: true}
	}
}

func rollbackNetworkSettings(ctx context.Context, service networking.Service, desired networking.Config, expectedRevision string) error {
	_, err := service.Apply(ctx, networking.Plan{Baseline: expectedRevision, Desired: desired})
	return err
}
func hasLiveHTTP(snapshot monitor.Snapshot) bool {
	for _, service := range snapshot.Services {
		if service.Protocol == "http" && (service.State == "running" || service.State == "ready") {
			return true
		}
	}
	return false
}
func (m *app) ensureLocalProxy() tea.Cmd {
	return func() tea.Msg {
		configPath, err := routing.DefaultPath()
		if err != nil {
			return proxyReadyMsg{err}
		}
		cfg, err := localrouting.Load(configPath)
		if err != nil {
			return proxyReadyMsg{err}
		}
		info, statusErr := proxy.Status(m.store)
		proxyRunning := statusErr == nil && info.State == "ready"
		if statusErr != nil {
			return proxyReadyMsg{statusErr}
		}
		if proxyRunning {
			if err = proxy.PrepareReload(m.ctx, m.store); err != nil {
				return proxyReadyMsg{err}
			}
		}
		if err = runner.ReconcileLocalRoutes(m.ctx, m.store, cfg); err != nil {
			if proxyRunning {
				_ = proxy.AbortReload(m.ctx, m.store)
			}
			return proxyReadyMsg{err}
		}
		exe, err := os.Executable()
		if err != nil {
			return proxyReadyMsg{err}
		}
		err = proxy.Ensure(m.ctx, m.store, exe, configPath)
		if err != nil && proxyRunning {
			_ = proxy.AbortReload(m.ctx, m.store)
		}
		return proxyReadyMsg{err}
	}
}
func tick() tea.Cmd          { return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }
func (m *app) Init() tea.Cmd { return tea.Batch(m.read, tick()) }

func (m *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.view.Width, m.view.Height = m.width, max(1, m.height-5)
		m.syncView()
	case tickMsg:
		cmds := []tea.Cmd{tick()}
		if !m.reading {
			m.reading = true
			cmds = append(cmds, m.read)
		}
		if m.ShowLogs && !m.logReading {
			cmds = append(cmds, m.readLogs())
		}
		return m, tea.Batch(cmds...)
	case snapshotMsg:
		m.reading = false
		if msg.err != nil {
			m.Status = "Refresh failed: " + msg.err.Error()
		} else {
			m.Apply(msg.snapshot)
			m.syncView()
			if !m.proxyEnsured && !m.proxyEnsuring && hasLiveHTTP(msg.snapshot) {
				m.proxyEnsuring = true
				return m, tea.Batch(m.ensureLocalProxy(), tick())
			}
		}
	case proxyReadyMsg:
		m.proxyEnsuring = false
		m.proxyEnsured = msg.err == nil
		if msg.err != nil {
			m.Status = "Local proxy: " + msg.err.Error()
		} else {
			m.Status = "Local proxy ready"
		}
		m.syncView()
	case setupMsg:
		m.networkBusy = false
		if msg.partial != "" {
			m.setupResult = &msg.result
			m.setupPartial = msg.partial
			m.Status = msg.partial
		} else if msg.err != nil {
			m.Status = "Tailnet setup: " + msg.err.Error()
		} else if msg.checked {
			m.setupStatus = msg.status
			m.proxyInfo = msg.proxyInfo
			m.Status = "Networking check completed"
		} else if msg.planned {
			m.setupPlan = &msg.plan
			m.view.GotoTop()
			m.Status = "External setup plan ready. Review terms before apply."
		} else if msg.applied {
			m.setupResult = &msg.result
			m.Status = "Tailnet setup applied"
		}
		m.syncView()
	case logsMsg:
		m.logReading = false
		if !m.ShowLogs || msg.id != m.logService.ID || msg.run != m.logService.RunID {
			return m, nil
		}
		if msg.err != nil {
			m.Status = "Logs: " + msg.err.Error()
		} else {
			bottom := m.view.AtBottom() || m.Logs == ""
			m.Logs = safe(msg.text)
			m.syncView()
			if bottom {
				m.view.GotoBottom()
			}
		}
	case actionMsg:
		if msg.stop {
			m.stopping = false
			if msg.err == nil {
				m.Status = "Process stopped"
			} else {
				m.Status = "Stop failed: " + msg.err.Error()
			}
		} else if msg.err != nil {
			m.Status = "Could not open URL: " + msg.err.Error()
		}
		if !m.reading {
			m.reading = true
			return m, m.read
		}
	case networkMsg:
		m.networkBusy = false
		if msg.err != nil {
			if msg.planned {
				m.networkPlan = nil
				m.routePreview = nil
			}
			m.Status = "Networking: " + msg.err.Error()
		} else {
			if !msg.planned {
				m.networkSnapshot = msg.snapshot
			}
			if msg.planned {
				m.networkPlan = &msg.plan
				m.routePreview = msg.routeChanges
				m.routeRevision = msg.routeRevision
			}
			if msg.apply {
				m.Status = "Networking settings saved"
				return m, m.inspectNetwork()
			}
			if msg.plan.Baseline != "" {
				m.Status = "Plan ready. Press Enter to apply, or Esc to discard."
			} else {
				m.Status = "Networking settings loaded"
			}
		}
		m.syncView()
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.ShowLogs {
			if key == "q" || key == "esc" {
				m.ShowLogs = false
				m.Logs = ""
				m.view.GotoTop()
				m.syncView()
				return m, nil
			}
			var cmd tea.Cmd
			m.view, cmd = m.view.Update(msg)
			return m, cmd
		}
		if m.showNetwork {
			if m.credentialEdit {
				switch key {
				case "esc":
					for i := range m.credentialInput {
						m.credentialInput[i] = 0
					}
					m.credentialInput = nil
					m.credentialEdit = false
				case "enter":
					if len(m.credentialInput) == 0 {
						m.Status = "Credential is empty"
						break
					}
					err := m.network.StoreCredential("cloudflare-token", m.credentialInput)
					for i := range m.credentialInput {
						m.credentialInput[i] = 0
					}
					m.credentialInput = nil
					m.credentialEdit = false
					if err != nil {
						m.Status = "Credential: " + err.Error()
					} else {
						m.setupPlan = nil
						m.networkSnapshot.Config.Tailnet.CredentialRef = "cloudflare-token"
						m.networkPlan = nil
						m.Status = "Credential stored privately. Press p to save its reference."
					}
				case "backspace":
					if len(m.credentialInput) > 0 {
						m.credentialInput = m.credentialInput[:len(m.credentialInput)-1]
					}
				default:
					if len(key) == 1 && key[0] >= 32 && key[0] <= 126 {
						m.credentialInput = append(m.credentialInput, key[0])
					}
				}
				m.syncView()
				return m, nil
			}
			if m.networkEdit {
				switch key {
				case "esc":
					m.networkEdit = false
				case "enter":
					m.networkEdit = false
					m.networkPlan = nil
					m.Status = "Setting changed. Press p to preview the settings."
				case "backspace":
					value := m.networkText()
					if len(value) > 0 {
						m.setNetworkText(value[:len(value)-1])
					}
				default:
					if len(key) == 1 && (key[0] >= 'a' && key[0] <= 'z' || key[0] >= 'A' && key[0] <= 'Z' || key[0] >= '0' && key[0] <= '9' || strings.ContainsRune("@._-", rune(key[0]))) {
						m.setNetworkText(m.networkText() + key)
					}
				}
				m.syncView()
				return m, nil
			}
			switch key {
			case "esc", "n":
				m.showNetwork, m.networkPlan = false, nil
			case "r":
				return m, m.inspectNetwork()
			case "v":
				return m, m.checkNetwork()
			case "x":
				m.setupPlan = nil
				m.view.GotoTop()
				return m, m.planSetup()
			case "c":
				m.credentialEdit = true
				m.credentialInput = nil
				m.Status = "Enter a scoped Cloudflare DNS token. Input is masked."
			case "A":
				if m.setupPlan != nil {
					if m.setupPlan.TermsURL != "" && !m.termsVisible() {
						m.Status = "Scroll until the ACME terms are visible before accepting them."
						break
					}
					m.setupPlan.TermsAccepted = true
					m.Status = "Terms accepted for this setup plan. Press Enter to apply."
				}
			case "p":
				if !m.networkBusy {
					cfg := m.networkSnapshot.Config
					revision := m.networkSnapshot.Revision
					m.networkBusy = true
					return m, func() tea.Msg {
						configPath, err := routing.DefaultPath()
						if err != nil {
							return networkMsg{err: err, planned: true}
						}
						current, err := localrouting.Load(configPath)
						if err != nil {
							return networkMsg{err: err, planned: true}
						}
						routes, err := runner.PlanLocalRoutes(m.ctx, m.store, localrouting.WithDesired(current, cfg))
						if err != nil {
							return networkMsg{err: err, planned: true}
						}
						plan, err := m.network.PlanAtRevision(m.ctx, cfg, revision)
						return networkMsg{plan: plan, err: err, planned: true, routeChanges: routes.Changes, routeRevision: routes.Revision}
					}
				}
			case "pgdown", "ctrl+f":
				var cmd tea.Cmd
				m.view, cmd = m.view.Update(msg)
				return m, cmd
			case "pgup", "ctrl+b":
				var cmd tea.Cmd
				m.view, cmd = m.view.Update(msg)
				return m, cmd
			case "enter":
				if m.setupPlan != nil && !m.networkBusy {
					if m.setupPlan.TermsURL != "" && !m.setupPlan.TermsAccepted {
						m.Status = "Review the ACME terms and press A to accept before applying."
						break
					}
					return m, m.applySetup(*m.setupPlan)
				}
				if m.networkPlan != nil && m.networkPlan.Baseline != "" && !m.networkBusy {
					plan := *m.networkPlan
					return m, m.applyNetworkSettings(plan, m.routeRevision, m.routePreview)
				}
			case "d":
				m.setupPlan = nil
				m.beginNetworkText("domain", m.networkSnapshot.Config.Tailnet.Domain, "Type a domain")
			case "z":
				m.setupPlan = nil
				m.beginNetworkText("zone", m.networkSnapshot.Config.Tailnet.ZoneID, "Type the Cloudflare zone ID")
			case "m":
				m.setupPlan = nil
				m.beginNetworkText("email", m.networkSnapshot.Config.Tailnet.Email, "Type the ACME registration email")
			case "t":
				m.setupPlan = nil
				if s, ok := m.SelectedService(); ok {
					cfg := m.networkSnapshot.Config
					project := cfg.Projects[s.ProjectID]
					project.Tailnet = !project.Tailnet
					cfg.Projects[s.ProjectID] = project
					m.networkSnapshot.Config = cfg
					m.networkPlan = nil
					m.Status = "Project exposure changed. Press p to preview the settings."
				}
			case "s":
				m.setupPlan = nil
				if s, ok := m.SelectedService(); ok {
					cfg := m.networkSnapshot.Config
					project := cfg.Projects[s.ProjectID]
					project.DefaultService = s.Name
					cfg.Projects[s.ProjectID] = project
					m.networkSnapshot.Config = cfg
					m.networkPlan = nil
					m.Status = "Default service changed. Press p to preview."
				}
			case "w":
				m.setupPlan = nil
				if s, ok := m.SelectedService(); ok {
					cfg := m.networkSnapshot.Config
					project := cfg.Projects[s.ProjectID]
					project.PrimaryWorktree = s.WorktreeID
					cfg.Projects[s.ProjectID] = project
					m.networkSnapshot.Config = cfg
					m.networkPlan = nil
					m.Status = "Primary worktree changed. Press p to preview."
				}
			case "up", "k":
				m.Move(-1)
			case "down", "j":
				m.Move(1)
			case "+", "shift+=", "-":
				m.setupPlan = nil
				cfg := m.networkSnapshot.Config
				_, p, _ := strings.Cut(cfg.Local.Listen, ":")
				port, _ := strconv.Atoi(p)
				if port < 1024 {
					port = 8080
				}
				if key == "+" || key == "shift+=" {
					port++
				} else if port > 1024 {
					port--
				}
				cfg.Local.Listen = fmt.Sprintf("127.0.0.1:%d", port)
				m.networkSnapshot.Config = cfg
				m.networkPlan = nil
				m.Status = "Local port changed. Press p to preview the settings."
			}
			m.syncView()
			return m, nil
		}
		switch key {
		case "q":
			return m, tea.Quit
		case "n":
			m.showNetwork = true
			m.networkPlan = nil
			m.view.GotoTop()
			return m, m.inspectNetwork()
		case "up", "k":
			m.Move(-1)
		case "down", "j":
			m.Move(1)
		case "tab", "shift+tab":
			m.ToggleTab()
			m.view.GotoTop()
		case "a":
			m.ToggleScope()
			m.view.GotoTop()
		case "l":
			if s, ok := m.SelectedService(); ok && !m.logReading {
				m.ShowLogs, m.Logs, m.logService = true, "", s
				m.view.GotoTop()
				m.syncView()
				return m, m.readLogs()
			}
		case "s":
			if s, ok := m.SelectedService(); ok && !m.stopping {
				m.stopping = true
				m.Status = "Stopping " + s.Name + "…"
				return m, func() tea.Msg { return actionMsg{stop: true, err: monitor.Stop(m.ctx, m.store, s)} }
			}
		case "o":
			if s, ok := m.SelectedService(); ok {
				u := s.URL
				if u == "" {
					u = s.DirectURL
				}
				if u == "" {
					m.Status = "Service has no URL"
				} else {
					return m, func() tea.Msg { return actionMsg{err: openURL(m.ctx, u)} }
				}
			}
		case "O":
			if s, ok := m.SelectedService(); ok {
				if s.TailnetURL == "" {
					m.Status = "Tailnet URL is not enabled for this project"
				} else {
					return m, func() tea.Msg { return actionMsg{err: openURL(m.ctx, s.TailnetURL)} }
				}
			}
		}
		m.syncView()
	}
	return m, nil
}

func (m *app) syncView() {
	if m.ShowLogs {
		m.view.SetContent(m.Logs)
		return
	}
	if m.showNetwork {
		m.view.SetContent(networkLines(m))
		return
	}
	lines, selected := listLines(m.Model)
	for i, line := range lines {
		line = clip(line, m.width)
		if i == selected {
			line = m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(line)
		} else if len(line) > 0 && line[0] != ' ' {
			line = m.renderer.NewStyle().Bold(true).Render(line)
		}
		lines[i] = line
	}
	m.view.SetContent(strings.Join(lines, "\n"))
	if selected >= 0 {
		if selected < m.view.YOffset {
			m.view.SetYOffset(selected)
		} else if selected >= m.view.YOffset+m.view.Height {
			m.view.SetYOffset(selected - m.view.Height + 1)
		}
	}
}

func (m *app) View() string {
	brand := m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render("rdrtop")
	scope := "all projects"
	if !m.AllProjects && m.Snapshot.CurrentProjectID != "" {
		scope = "current project"
	}
	var tabs []string
	for i, name := range []string{"Running", "Last launch errors"} {
		model := m.Model
		model.Tab = i
		style := m.renderer.NewStyle().Padding(0, 1)
		if m.Tab == i {
			style = style.Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6"))
		} else {
			style = style.Faint(true)
		}
		tabs = append(tabs, style.Render(fmt.Sprintf("%s (%d)", name, len(model.rows()))))
	}
	status := m.Status
	if len(m.Snapshot.Warnings) > 0 {
		status += "  Warning: " + m.Snapshot.Warnings[0]
	}
	footer := "↑/k ↓/j select · Tab tabs · a scope · l logs · o URL · s stop · q quit"
	detail := ""
	if s, ok := m.SelectedService(); ok {
		detail = s.URL
		if s.MissingWorktree {
			footer = "s Stop process without worktree · ↑/k ↓/j select · Tab tabs · a scope · l logs · o URL · q quit"
		}
	}
	if m.ShowLogs {
		footer = "↑/k ↓/j scroll · PgUp/PgDown page · q/Esc back · Ctrl-C quit"
		detail = "Logs · " + m.logService.Name + " · " + m.logService.Worktree
	}
	if m.showNetwork {
		footer = "↑/k ↓/j service · PgUp/PgDn scroll · +/- local port · d domain · z zone · m ACME email · t tailnet · s default · w primary · c token · p preview · x setup · v check · Enter apply · Esc back"
		detail = "Setup is explicit. Review the plan before applying provider changes."
	}
	lines := []string{brand + "  ·  " + scope, strings.Join(tabs, " "), clip(status, m.width), m.view.View(), clip(detail, m.width), clip(footer, m.width)}
	for i := range lines {
		if i != 3 {
			lines[i] = ansi.Truncate(lines[i], m.width, "…")
		}
	}
	return lipgloss.NewStyle().MaxHeight(m.height).Render(strings.Join(lines, "\n"))
}

func networkLines(m *app) string {
	c := m.networkSnapshot.Config
	lines := []string{"Networking", "Local HTTP: " + c.Local.Listen + " (always enabled)", "Tailnet domain: " + valueOr(c.Tailnet.Domain, "not configured"), "Cloudflare credential: " + valueOr(c.Tailnet.CredentialRef, "not configured"), ""}
	projects := map[string]string{}
	for _, s := range m.Snapshot.Services {
		projects[s.ProjectID] = s.ProjectName
	}
	ids := make([]string, 0, len(projects))
	for id := range projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if s, ok := m.SelectedService(); ok {
		lines = append(lines, "Selected project: "+s.ProjectName+" ["+s.ProjectID+"]")
	}
	for _, id := range ids {
		state := "local only"
		if c.Projects[id].Tailnet {
			state = "tailnet enabled"
		}
		lines = append(lines, "  "+projects[id]+"  "+state)
	}
	if m.networkPlan != nil {
		lines = append(lines, "", "Settings preview", "  Based on config revision: "+m.networkPlan.Baseline)
		if len(m.networkPlan.Changes) == 0 {
			lines = append(lines, "  No settings changes")
		}
		for _, change := range m.networkPlan.Changes {
			lines = append(lines, "  "+change)
		}
		lines = append(lines, fmt.Sprintf("  Routes based on registry revision %d:", m.routeRevision))
		if len(m.routePreview) == 0 {
			lines = append(lines, "    No endpoint or alias changes")
		}
		for _, route := range m.routePreview {
			action := "Add"
			if route.Action == "remove" {
				action = "Remove"
			}
			alias := "canonical"
			if route.Alias {
				alias = "project alias"
			}
			lines = append(lines, fmt.Sprintf("    %s %s %s://%s:%d owner=%s instance=%s (%s)", action, route.Scope, routeScheme(route.Scope), route.Hostname, route.Port, route.ProjectID, route.InstanceID, alias))
		}
	}
	if m.networkEdit {
		lines = append(lines, "", m.textField+": "+m.networkText()+"_")
	}
	if m.credentialEdit {
		lines = append(lines, "", "Cloudflare token: "+strings.Repeat("•", len(m.credentialInput))+"_")
	}
	if m.setupStatus.Domain != "" || m.setupStatus.DNS != "" || m.proxyInfo.RenewalError != "" || m.proxyInfo.CertificateActivationPending {
		lines = append(lines, "", "Tailnet status", "  DNS: "+valueOr(m.setupStatus.DNS, "unknown"), "  Certificate: "+valueOr(m.setupStatus.Certificate, "unknown"), "  Tailscale Serve: "+valueOr(m.setupStatus.Serve, "unknown"))
		if m.proxyInfo.CertificateActivationPending {
			lines = append(lines, "  Certificate activation: pending")
		}
		if m.proxyInfo.RenewalError != "" {
			lines = append(lines, "  Renewal / activation error: "+safe(m.proxyInfo.RenewalError))
		}
		if !m.setupStatus.Expiry.IsZero() {
			lines = append(lines, "  Certificate expires: "+m.setupStatus.Expiry.Format(time.RFC3339))
		}
		for _, diagnostic := range m.setupStatus.Diagnostics {
			lines = append(lines, "  "+diagnostic)
		}
	}
	if m.setupPlan != nil {
		lines = append(lines, "", "External setup plan", "  Domain: "+m.setupPlan.Domain, "  DNS action: "+m.setupPlan.DNSAction, "  DNS zone: "+m.setupPlan.DNS.Zone, "  DNS name: "+m.setupPlan.DNS.Desired.Name, "  DNS target: "+m.setupPlan.DNS.Desired.Content, "  DNS proxied: "+strconv.FormatBool(m.setupPlan.DNS.Desired.Proxied), "  Serve: "+m.setupPlan.ServeAction, "  Certificate: "+m.setupPlan.CertificateAction)
		if m.setupPlan.TermsURL != "" {
			lines = append(lines, "  ACME terms: "+m.setupPlan.TermsURL, "  Press A after review to accept for this plan.")
		}
	}
	if m.setupResult != nil {
		for _, step := range m.setupResult.Steps {
			lines = append(lines, "  "+step)
		}
	}
	if m.networkBusy {
		lines = append(lines, "", "Working…")
	}
	return strings.Join(lines, "\n")
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func routeScheme(scope string) string {
	if scope == "tailnet" {
		return "https"
	}
	return "http"
}

func (m *app) termsVisible() bool {
	if m.setupPlan == nil || m.setupPlan.TermsURL == "" {
		return true
	}
	for i, line := range strings.Split(networkLines(m), "\n") {
		if strings.Contains(line, "ACME terms:") {
			return i >= m.view.YOffset && i < m.view.YOffset+m.view.Height
		}
	}
	return false
}

func (m *app) networkText() string {
	switch m.textField {
	case "domain":
		return m.networkSnapshot.Config.Tailnet.Domain
	case "zone":
		return m.networkSnapshot.Config.Tailnet.ZoneID
	case "email":
		return m.networkSnapshot.Config.Tailnet.Email
	default:
		return ""
	}
}

func (m *app) setNetworkText(value string) {
	switch m.textField {
	case "domain":
		m.networkSnapshot.Config.Tailnet.Domain = value
	case "zone":
		m.networkSnapshot.Config.Tailnet.ZoneID = value
	case "email":
		m.networkSnapshot.Config.Tailnet.Email = value
	}
}

func (m *app) beginNetworkText(field, current, prompt string) {
	m.textField = field
	m.networkEdit = true
	m.setNetworkText(current)
	m.Status = prompt + ", then Enter to keep it or Esc to cancel."
}

func listLines(m Model) ([]string, int) {
	var lines []string
	previous, selected := "", -1
	for _, s := range m.rows() {
		if s.ProjectID != previous {
			lines = append(lines, safe(s.ProjectName)+"  ["+safe(s.ProjectID[:min(10, len(s.ProjectID))])+"]")
			previous = s.ProjectID
		}
		cursor := " "
		if s.ID == m.Selected {
			cursor, selected = ">", len(lines)
		}
		worktree := s.Branch
		if worktree == "" {
			worktree = s.Worktree
		}
		if s.MissingWorktree {
			worktree += " [worktree removed]"
		}
		age := "-"
		if !s.StartedAt.IsZero() {
			age = time.Since(s.StartedAt).Round(time.Second).String()
		}
		ports := monitor.PortLabel(s.Ports)
		if ports == "" {
			ports = "-"
		}
		lines = append(lines, fmt.Sprintf(" %s %s  [%s]  %s  pid %d  port %s  %s", cursor, safe(s.Name), safe(worktree), safe(s.State), s.PID, ports, age))
		if m.Tab == 1 {
			lines = append(lines, "    Error: "+safe(s.Error))
		}
		if s.URL != "" {
			lines = append(lines, "    "+safe(s.URL))
		}
		if s.TailnetURL != "" {
			lines = append(lines, "    Tailnet: "+safe(s.TailnetURL))
		}
		if s.LocalAliasURL != "" {
			lines = append(lines, "    Project alias: "+safe(s.LocalAliasURL))
		}
		if s.TailnetAliasURL != "" {
			lines = append(lines, "    Tailnet alias: "+safe(s.TailnetAliasURL))
		}
		if s.ReportedURL != "" {
			lines = append(lines, "    App reported: "+safe(s.ReportedURL))
		}
		if s.LaunchURLStale {
			lines = append(lines, "    Running with older launch URL values. Restart to update the app environment.")
		}
	}
	if len(lines) == 0 {
		message := "No running rdr services."
		if m.Tab == 1 {
			message = "No last launch errors."
		}
		lines = append(lines, message)
	}
	return lines, selected
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return '�'
		}
		return r
	}, s)
}
func clip(s string, width int) string {
	s = strings.NewReplacer("\n", " ", "\t", "    ").Replace(safe(s))
	return ansi.Truncate(s, max(0, width), "…")
}
func openURL(ctx context.Context, url string) error {
	if runtime.GOOS == "darwin" {
		return exec.CommandContext(ctx, "open", url).Run()
	}
	return exec.CommandContext(ctx, "xdg-open", url).Run()
}
