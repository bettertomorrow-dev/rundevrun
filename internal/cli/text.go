// Formats discovery, plan, and administrative results for terminal readers.
package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
)

func singleLine(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, value)
}

func displayURLs(addresses []string) []string {
	var urls []string
	for _, address := range addresses {
		address = singleLine(address)
		if address != "" && !slices.Contains(urls, address) {
			urls = append(urls, address)
		}
	}
	return urls
}

func worktreeName(branch, root string) string {
	if branch != "" {
		return branch
	}
	if root != "" {
		return filepath.Base(root)
	}
	return ""
}

func (e *envelope) rememberWorktree(projectID, project, worktreeID, branch, root string) {
	name := worktreeName(branch, root)
	if name == "" {
		return
	}
	if e.worktreeLabels == nil {
		e.worktreeLabels = make(map[string]string)
	}
	if project != "" {
		name = project + "/" + name
	}
	e.worktreeLabels[projectID+"/"+worktreeID] = name
}

func (e envelope) instanceLabel(id string) string {
	if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
		if label := e.worktreeLabels[id[:slash]]; label != "" {
			return singleLine(serviceName(id) + " (" + label + ")")
		}
	}
	return singleLine(id)
}

func textStatus(state string, color bool) string {
	switch state {
	case "ready", "running":
		return terminalSymbol("●", "32", color)
	case "starting":
		return terminalSymbol("○", "36", color)
	case "failed", "unhealthy":
		return lifecycleSymbol("failed", color)
	case "stopped":
		return lifecycleSymbol("not_run", color)
	default:
		return lifecycleSymbol("", color)
	}
}

func writeTextResults(out io.Writer, e envelope) error {
	var b strings.Builder
	switch e.Command {
	case "status", "ls", "services":
		writeServiceList(&b, e, colorEnabled(out))
	case "projects", "register", "worktrees", "config check":
		writeDiscovery(&b, e)
	case "switch":
		if !e.DryRun {
			writeSwitchResult(&b, e, colorEnabled(out))
		}
	default:
		for _, item := range e.Results {
			writeTextItem(&b, e, item)
		}
	}
	writeTextPlan(&b, e)
	if strings.HasPrefix(e.Command, "proxy ") && e.Proxy != nil {
		writeProxy(&b, e.Proxy)
	}
	if b.Len() == 0 {
		return nil
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func writeServiceList(b *strings.Builder, e envelope, color bool) {
	if len(e.Results) == 0 {
		if len(e.Errors) == 0 {
			b.WriteString("No services.\n")
		}
		return
	}
	rows := make([]instanceInfo, 0, len(e.Results))
	multipleProjects := false
	for _, result := range e.Results {
		if item, ok := result.(instanceInfo); ok {
			if len(rows) > 0 && item.ProjectID != rows[0].ProjectID {
				multipleProjects = true
			}
			rows = append(rows, item)
		}
	}
	headers := [3]string{"SERVICE", "WORKTREE", "STATE"}
	widths := [3]int{len(headers[0]), len(headers[1]), len(headers[2])}
	cells := make([][3]string, 0, len(rows))
	for _, row := range rows {
		worktree := row.worktreeLabel
		if worktree == "" {
			worktree = row.WorktreeID
		}
		if multipleProjects && row.projectName != "" {
			worktree = row.projectName + "/" + worktree
		}
		state := row.State
		if row.StateUnknown || state == "" {
			state = "unknown"
		}
		values := [3]string{singleLine(row.Service), singleLine(worktree), singleLine(state)}
		for i, value := range values {
			widths[i] = max(widths[i], utf8.RuneCountInString(value))
		}
		cells = append(cells, values)
	}
	prefix := func(mark string, values [3]string) string {
		return fmt.Sprintf("%s %-*s  %-*s  %-*s", mark, widths[0], values[0], widths[1], values[1], widths[2], values[2])
	}
	fmt.Fprintln(b, prefix(" ", headers)+"  URL")
	indent := strings.Repeat(" ", widths[0]+widths[1]+widths[2]+8)
	for i, row := range rows {
		line := prefix(textStatus(cells[i][2], color), cells[i])
		urls := displayURLs(row.displayURLs)
		if len(urls) == 0 {
			fmt.Fprintln(b, strings.TrimRight(line, " "))
		} else {
			fmt.Fprintln(b, line+"  "+urls[0])
			for _, address := range urls[1:] {
				fmt.Fprintln(b, indent+address)
			}
		}
		for _, note := range []string{row.ConfigError, row.DependencyError} {
			if note != "" {
				fmt.Fprintf(b, "  %s\n", singleLine(note))
			}
		}
		if row.RestartRequired {
			b.WriteString("  Restart required.\n")
		}
	}
}

func writeDiscovery(b *strings.Builder, e envelope) {
	t := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	switch e.Command {
	case "config check":
		if len(e.Errors) == 0 {
			for _, item := range e.Results {
				if info, ok := item.(instanceInfo); ok {
					fmt.Fprintf(b, "Configuration valid for %s.\n", singleLine(info.worktreeLabel))
					return
				}
			}
			b.WriteString("Configuration valid.\n")
		}
	case "projects":
		if len(e.Results) > 0 {
			fmt.Fprintln(t, "PROJECT\tPATH\tID")
		} else if len(e.Errors) == 0 {
			b.WriteString("No registered projects.\n")
		}
	case "worktrees":
		if len(e.Results) > 0 {
			fmt.Fprintln(t, "WORKTREE\tPATH\tCONFIG")
		}
	}
	for _, item := range e.Results {
		switch v := item.(type) {
		case projectInfo:
			if e.Command == "register" {
				action := "Registered"
				switch {
				case e.DryRun:
					action = "Would register"
				case len(e.Errors) > 0:
					action = "Project"
				}
				fmt.Fprintf(t, "%s %s\t%s\t%s\n\nWORKTREE\tPATH\tCONFIG\n", action, singleLine(v.Name), singleLine(v.Root), singleLine(v.ID))
			} else {
				fmt.Fprintf(t, "%s\t%s\t%s\n", singleLine(v.Name), singleLine(v.Root), singleLine(v.ID))
			}
		case worktreeInfo:
			config := "valid"
			if !v.ConfigValid {
				config = "missing dev.toml"
			}
			if v.ConfigError != "" {
				config = v.ConfigError
			}
			name := worktreeName(v.Branch, v.Root)
			if v.Detached {
				name += " (detached)"
			}
			fmt.Fprintf(t, "%s\t%s\t%s\n", singleLine(name), singleLine(v.Root), singleLine(config))
		}
	}
	_ = t.Flush()
}

func writeTextItem(b *strings.Builder, e envelope, item any) {
	switch v := item.(type) {
	case runner.BindResult:
		fmt.Fprintf(b, "Bind %s to %s\n", e.instanceLabel(v.ConsumerID), e.instanceLabel(v.TargetID))
		writeRestartNote(b, v.RestartRequired)
	case runner.AliasPlan:
		fmt.Fprintf(b, "Worktree alias  %s\n", singleLine(v.NewAlias))
		for _, change := range v.Changes {
			writeAliasChange(b, e, change.InstanceID, change.OldURL, change.NewURL, change.RestartRequired)
		}
	case runner.HostnameAliasPlan:
		writeAliasChange(b, e, v.InstanceID, v.OldURL, v.NewURL, v.RestartRequired)
	case networking.Snapshot:
		fmt.Fprintf(b, "Local listener  %s\nConnected domain  %s\n", singleLine(v.Config.Local.Listen), configuredDomain(v.Config.Tailnet.Domain))
	case networking.SetupStatus:
		writeNetworkStatus(b, v)
	case networking.SetupResult:
		for _, step := range v.Steps {
			fmt.Fprintln(b, singleLine(step))
		}
		writeNetworkStatus(b, v.Status)
	default:
		fmt.Fprintln(b, item)
	}
}

func writeRestartNote(b *strings.Builder, required bool) {
	if required {
		b.WriteString("  Restart required.\n")
	}
}

func writeAliasChange(b *strings.Builder, e envelope, id, oldURL, newURL string, restart bool) {
	if oldURL != "" && oldURL != newURL {
		fmt.Fprintf(b, "%s  %s → %s\n", e.instanceLabel(id), singleLine(oldURL), singleLine(newURL))
	} else {
		fmt.Fprintf(b, "%s  %s\n", e.instanceLabel(id), singleLine(newURL))
	}
	writeRestartNote(b, restart)
}

func configuredDomain(domain string) string {
	if domain == "" {
		return "not configured"
	}
	return singleLine(domain)
}

func writeNetworkStatus(b *strings.Builder, status networking.SetupStatus) {
	fmt.Fprintf(b, "Connected domain  %s\nDNS  %s\nTailscale Serve  %s\nCertificate  %s\n",
		configuredDomain(status.Domain), singleLine(status.DNS), singleLine(status.Serve), singleLine(status.Certificate))
	if !status.Expiry.IsZero() {
		fmt.Fprintf(b, "Expires  %s\n", status.Expiry.Format("2006-01-02"))
	}
	for _, diagnostic := range status.Diagnostics {
		fmt.Fprintf(b, "  %s\n", singleLine(diagnostic))
	}
}

func writeProxy(b *strings.Builder, p *proxyInfoView) {
	fmt.Fprintf(b, "Proxy  %s\n", singleLine(p.State))
	for _, field := range [][2]string{
		{"HTTP", p.HTTPListen}, {"HTTPS", p.HTTPSListen}, {"TLS", p.TLSMode}, {"Pending listener", p.PreparedListen},
		{"TLS error", p.TLSError}, {"Renewal error", p.RenewalError},
	} {
		if field[1] != "" {
			fmt.Fprintf(b, "  %s  %s\n", field[0], singleLine(field[1]))
		}
	}
	if p.CertificateActivationPending {
		b.WriteString("  Certificate activation pending.\n")
	}
}

func writeTextPlan(b *strings.Builder, e envelope) {
	switch plan := e.Plan.(type) {
	case runner.GraphPlan:
		fmt.Fprintf(b, "Plan for rdr %s\n", singleLine(plan.Action))
		if len(plan.Instances) == 0 {
			b.WriteString("  No services selected.\n")
		}
		for _, instance := range plan.Instances {
			steps := "Keep " + instance.State
			if len(instance.Steps) > 0 {
				labels := make([]string, 0, len(instance.Steps))
				for _, step := range instance.Steps {
					labels = append(labels, stepLabel(step))
				}
				steps = strings.Join(labels, " → ")
			}
			fmt.Fprintf(b, "  %s  %s\n", e.instanceLabel(instance.InstanceID), singleLine(steps))
			if address := instance.Addresses["url"]; address != "" && slices.Contains(instance.Steps, "start") {
				fmt.Fprintf(b, "    %s\n", singleLine(address))
			}
		}
		writePublicationPlan(b, plan.Publications)
	case runner.SwitchPlan:
		b.WriteString("Plan for rdr switch\n")
		for _, pair := range plan.Mappings {
			fmt.Fprintf(b, "  %s → %s\n", e.instanceLabel(pair.From), e.instanceLabel(pair.To))
		}
		for _, step := range plan.Steps {
			fmt.Fprintf(b, "  %s  %s\n", stepLabel(step.Action), e.instanceLabel(step.InstanceID))
		}
		writePublicationPlan(b, plan.Publications)
	case runner.BindResult, runner.AliasPlan, runner.HostnameAliasPlan:
		fmt.Fprintf(b, "Plan for rdr %s\n", e.Command)
		writeTextItem(b, e, plan)
	case networkPlanFile:
		b.WriteString("Network plan\n")
		writeTextItem(b, e, networking.Snapshot{Config: plan.Settings.Desired})
		if len(plan.Settings.Changes) == 0 && len(plan.RouteChanges) == 0 {
			b.WriteString("  No changes.\n")
		}
		for _, change := range plan.Settings.Changes {
			fmt.Fprintf(b, "  %s\n", singleLine(change))
		}
		for _, change := range plan.RouteChanges {
			fmt.Fprintf(b, "  %s %s route  %s:%d  %s\n", stepLabel(change.Action), singleLine(change.Scope), singleLine(change.Hostname), change.Port, e.instanceLabel(change.InstanceID))
		}
	case networking.SetupPlan:
		fmt.Fprintf(b, "Tailnet setup plan\nConnected domain  %s\n", configuredDomain(plan.Domain))
		fmt.Fprintf(b, "DNS  %s\nTailscale Serve  %s\nCertificate  %s\n", singleLine(plan.DNSAction), singleLine(plan.ServeAction), singleLine(plan.CertificateAction))
		if plan.DNS.Desired.Name != "" {
			fmt.Fprintf(b, "  %s → %s\n", singleLine(plan.DNS.Desired.Name), singleLine(plan.DNS.Desired.Content))
		}
		if plan.ServeTarget != "" {
			fmt.Fprintf(b, "Serve target  %s\n", singleLine(plan.ServeTarget))
		}
		if plan.TermsURL != "" && !plan.TermsAccepted {
			fmt.Fprintf(b, "Review terms  %s\n", singleLine(plan.TermsURL))
		}
	case map[string]any:
		if strings.HasPrefix(e.Command, "proxy ") {
			fmt.Fprintf(b, "Plan for rdr %s\n", e.Command)
			if proxy, ok := plan["proxy"].(*proxyInfoView); ok && proxy != nil {
				writeProxy(b, proxy)
			}
			if candidate, ok := plan["candidate"].(map[string]any); ok {
				b.WriteString("Proposed listeners\n")
				for _, field := range [][2]string{{"HTTP", "http_listen"}, {"HTTPS", "https_listen"}, {"TLS", "tls_mode"}} {
					if value, ok := candidate[field[1]].(string); ok && value != "" {
						fmt.Fprintf(b, "  %s  %s\n", field[0], singleLine(value))
					}
				}
			}
		}
	}
}

func stepLabel(step string) string {
	switch step {
	case "readiness":
		return "Wait for readiness"
	case "bind":
		return "Update binding"
	}
	value := singleLine(strings.ReplaceAll(step, "_", " "))
	if value == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(value)
	return string(unicode.ToUpper(first)) + value[size:]
}

func writePublicationPlan(b *strings.Builder, ids []string) {
	for _, id := range ids {
		fmt.Fprintf(b, "  Close share  %s\n", singleLine(id))
	}
}

func writeSwitchResult(b *strings.Builder, e envelope, color bool) {
	if len(e.Results) == 0 && len(e.Steps) == 0 && len(e.Errors) > 0 {
		return
	}
	status := "completed"
	switch e.Status {
	case "partial":
		status = "partially completed"
	case "error", "failed":
		status = "failed"
	}
	fmt.Fprintf(b, "Switch %s\n", status)
	for _, pair := range e.switchMappings {
		fmt.Fprintf(b, "  %s → %s\n", e.instanceLabel(pair.From), e.instanceLabel(pair.To))
	}
	for _, raw := range e.Results {
		if result, ok := raw.(runner.Result); ok {
			fmt.Fprintf(b, "%s %s  %s\n", lifecycleSymbol(result.Status, color), e.instanceLabel(result.InstanceID), singleLine(result.State))
		}
	}
	for _, raw := range e.Steps {
		step, ok := raw.(operationStepView)
		if !ok || step.Status == "completed" {
			continue
		}
		action, id, _ := strings.Cut(step.ID, ":")
		instance, detail, _ := strings.Cut(id, ":")
		message := e.instanceLabel(instance)
		if detail != "" {
			message += " / " + singleLine(detail)
		}
		if step.Message != "" {
			message += "  " + singleLine(step.Message)
		}
		fmt.Fprintf(b, "  %s %s  %s\n", lifecycleSymbol(step.Status, color), stepLabel(action), message)
	}
	if e.JournalPath != "" && e.Status != "ok" {
		fmt.Fprintf(b, "Journal  %s\n", singleLine(e.JournalPath))
	}
}
