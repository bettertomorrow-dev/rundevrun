// Formats command results and errors as text or JSON.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/bettertomorrow-dev/rundevrun/internal/publication"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/charmbracelet/x/term"
)

type scope struct {
	Project  string `json:"project,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

type commandError struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	InstanceID string   `json:"instance_id,omitempty"`
	Field      string   `json:"field,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Actions    []string `json:"actions"`
}

type envelope struct {
	Version           int                  `json:"version"`
	Command           string               `json:"command"`
	Scope             scope                `json:"scope"`
	Status            string               `json:"status"`
	DryRun            bool                 `json:"dry_run"`
	Results           []any                `json:"results"`
	Errors            []commandError       `json:"errors"`
	Plan              any                  `json:"plan,omitempty"`
	OperationID       string               `json:"operation_id,omitempty"`
	JournalPath       string               `json:"journal_path,omitempty"`
	Steps             []any                `json:"steps,omitempty"`
	Operations        []any                `json:"operations,omitempty"`
	Proxy             *proxyInfoView       `json:"proxy,omitempty"`
	ProxyErrors       []commandError       `json:"proxy_errors,omitempty"`
	RoutingErrors     []commandError       `json:"routing_errors,omitempty"`
	Credentials       *shareCredentials    `json:"credentials,omitempty"`
	Publications      []publication.Status `json:"publications,omitempty"`
	PublicationErrors []commandError       `json:"publication_errors,omitempty"`
	lifecycleURLs     map[string][]string
	text              bool
	worktreeLabels    map[string]string
	switchMappings    []runner.SwitchPair
}

type shareCredentials struct {
	Password string `json:"password"`
}

type proxyInfoView struct {
	State                        string `json:"state"`
	PID                          int    `json:"pid,omitempty"`
	HTTPListen                   string `json:"http_listen,omitempty"`
	HTTPSListen                  string `json:"https_listen,omitempty"`
	TLSMode                      string `json:"tls_mode,omitempty"`
	ConfigPath                   string `json:"config_path,omitempty"`
	PreparedListen               string `json:"prepared_listen,omitempty"`
	TLSError                     string `json:"tls_error,omitempty"`
	RenewalError                 string `json:"renewal_error,omitempty"`
	CertificateActivationPending bool   `json:"certificate_activation_pending,omitempty"`
}

func result(command string, opts options) envelope {
	e := envelope{Version: 1, Command: command, Scope: scope{opts.project, opts.worktree},
		Status: "ok", DryRun: opts.dryRun, Results: []any{}, Errors: []commandError{}, text: !opts.json}
	if (command == "up" || command == "restart") && !opts.json && !opts.dryRun {
		e.lifecycleURLs = make(map[string][]string)
	}
	return e
}

func (e *envelope) fail(code, message string, actions ...string) {
	e.Status = "error"
	if actions == nil {
		actions = []string{}
	}
	e.Errors = append(e.Errors, commandError{Code: code, Message: message, Actions: actions})
}

func writeResult(out, errOut io.Writer, e envelope, asJSON bool) int {
	if asJSON {
		if err := json.NewEncoder(out).Encode(e); err != nil {
			fmt.Fprintln(errOut, "rundevrun: cannot write result:", err)
			return 4
		}
	} else {
		lifecycle := e.Command == "up" || e.Command == "down" || e.Command == "restart"
		if lifecycle && !e.DryRun {
			if err := writeLifecycleTable(out, e); err != nil {
				fmt.Fprintln(errOut, "rundevrun: cannot write result:", err)
				return 4
			}
		} else {
			if err := writeTextResults(out, e); err != nil {
				fmt.Fprintln(errOut, "rundevrun: cannot write result:", err)
				return 4
			}
		}
		for _, err := range e.Errors {
			if e.Command == "switch" && err.InstanceID != "" {
				fmt.Fprintf(errOut, "rundevrun: %s: %s: %s\n", e.instanceLabel(err.InstanceID), err.Code, err.Message)
			} else if lifecycle && err.InstanceID != "" {
				fmt.Fprintf(errOut, "rundevrun: %s: %s: %s\n", singleLine(serviceName(err.InstanceID)), err.Code, err.Message)
			} else {
				fmt.Fprintf(errOut, "rundevrun: %s: %s\n", err.Code, err.Message)
			}
		}
		for _, err := range e.ProxyErrors {
			fmt.Fprintf(errOut, "rundevrun: proxy: %s: %s\n", err.Code, err.Message)
		}
		for _, err := range e.RoutingErrors {
			fmt.Fprintf(errOut, "rundevrun: routing: %s: %s\n", err.Code, err.Message)
		}
		for _, item := range e.Publications {
			if _, err := fmt.Fprintln(out, item.ID, publicationResult{Status: item}); err != nil {
				fmt.Fprintln(errOut, "rundevrun: cannot write result:", err)
				return 4
			}
		}
		for _, err := range e.PublicationErrors {
			fmt.Fprintf(errOut, "rundevrun: %s: %s\n", err.Code, err.Message)
		}
	}
	for _, err := range e.Errors {
		if err.Code == "CANCELLED" {
			return 130
		}
	}
	if e.Status == "partial" {
		return 5
	}
	if len(e.Errors) == 0 {
		return 0
	}
	switch e.Errors[0].Code {
	case "BUSY", "STATE_CONFLICT", "PORT_CONFLICT", "ROUTE_CONFLICT":
		return 3
	case "STATE_CORRUPT", "IO_ERROR", "BUILD_FAILED", "HOOK_FAILED", "READINESS_TIMEOUT", "PROCESS_FAILED", "OWNERSHIP_UNKNOWN", "TLS_INVALID", "TUNNEL_FAILED", "DIAGNOSTICS_FAILED", "REGISTRY_ERROR":
		return 4
	case "CANCELLED":
		return 130
	default:
		return 2
	}
}

func serviceName(id string) string {
	return id[strings.LastIndexByte(id, '/')+1:]
}

func colorEnabled(out io.Writer) bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	terminal, ok := out.(interface{ Fd() uintptr })
	return ok && !noColor && os.Getenv("TERM") != "dumb" && term.IsTerminal(terminal.Fd())
}

func lifecycleSymbol(status string, color bool) string {
	symbol, code := "?", "33"
	switch status {
	case "completed":
		symbol, code = "✓", "32"
	case "unchanged":
		symbol, code = "●", "36"
	case "failed":
		symbol, code = "✗", "31"
	case "not_run":
		symbol, code = "–", "90"
	}
	return terminalSymbol(symbol, code, color)
}

func terminalSymbol(symbol, code string, color bool) string {
	if color {
		return "\x1b[" + code + "m" + symbol + "\x1b[0m"
	}
	return symbol
}

func writeLifecycleTable(out io.Writer, e envelope) error {
	if len(e.Results) == 0 {
		return nil
	}
	color := colorEnabled(out)
	type row struct {
		service, symbol string
		urls            []string
		restart         bool
	}
	rows := make([]row, 0, len(e.Results))
	serviceWidth := len("SERVICE")
	for _, raw := range e.Results {
		var id, status string
		var restart bool
		switch item := raw.(type) {
		case runner.Result:
			id, status = item.InstanceID, item.Status
		case map[string]any:
			id, _ = item["instance_id"].(string)
			status, _ = item["status"].(string)
			restart, _ = item["restart_required"].(bool)
		}
		if e.Command == "down" && id == "" && status == "unchanged" {
			_, err := fmt.Fprintln(out, "No running services.")
			return err
		}
		urls := displayURLs(e.lifecycleURLs[id])
		service := singleLine(serviceName(id))
		serviceWidth = max(serviceWidth, utf8.RuneCountInString(service))
		rows = append(rows, row{service, lifecycleSymbol(status, color), urls, restart})
	}
	var table strings.Builder
	if e.Command == "down" {
		table.WriteString("  SERVICE\n")
	} else {
		fmt.Fprintf(&table, "  %-*s  URL\n", serviceWidth, "SERVICE")
	}
	urlIndent := strings.Repeat(" ", serviceWidth+4)
	for _, row := range rows {
		if len(row.urls) == 0 {
			fmt.Fprintf(&table, "%s %s\n", row.symbol, row.service)
		} else {
			fmt.Fprintf(&table, "%s %-*s  %s\n", row.symbol, serviceWidth, row.service, row.urls[0])
			for _, address := range row.urls[1:] {
				fmt.Fprintln(&table, urlIndent+address)
			}
		}
		if row.restart {
			fmt.Fprintf(&table, "  Restart required. Run rundevrun restart %s to apply changes.\n", row.service)
		}
	}
	_, err := io.WriteString(out, table.String())
	return err
}
