// Formats command results and errors as text or JSON.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bettertomorrow-dev/rundevrun/internal/publication"
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
	upURLs            map[string]upURLs
}

type upURLs struct {
	local, domain, reported string
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
		Status: "ok", DryRun: opts.dryRun, Results: []any{}, Errors: []commandError{}}
	if command == "up" && !opts.json && !opts.dryRun {
		e.upURLs = make(map[string]upURLs)
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
			fmt.Fprintln(errOut, "rdr: cannot write result:", err)
			return 4
		}
	} else {
		if e.Command == "up" && !e.DryRun {
			if err := writeUpTable(out, e); err != nil {
				fmt.Fprintln(errOut, "rdr: cannot write result:", err)
				return 4
			}
		} else {
			for _, item := range e.Results {
				if _, err := fmt.Fprintln(out, item); err != nil {
					fmt.Fprintln(errOut, "rdr: cannot write result:", err)
					return 4
				}
			}
		}
		for _, err := range e.Errors {
			if e.Command == "up" && err.InstanceID != "" {
				fmt.Fprintf(errOut, "rdr: %s: %s: %s\n", serviceName(err.InstanceID), err.Code, err.Message)
			} else {
				fmt.Fprintf(errOut, "rdr: %s: %s\n", err.Code, err.Message)
			}
		}
		if e.Command == "up" {
			for _, err := range e.ProxyErrors {
				fmt.Fprintf(errOut, "rdr: proxy: %s: %s\n", err.Code, err.Message)
			}
		}
		for _, item := range e.Publications {
			if _, err := fmt.Fprintln(out, item.ID, publicationResult{Status: item}); err != nil {
				fmt.Fprintln(errOut, "rdr: cannot write result:", err)
				return 4
			}
		}
		for _, err := range e.PublicationErrors {
			fmt.Fprintf(errOut, "rdr: %s: %s\n", err.Code, err.Message)
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

func upSymbol(status string, color bool) string {
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
	if color {
		return "\x1b[" + code + "m" + symbol + "\x1b[0m"
	}
	return symbol
}

func writeUpTable(out io.Writer, e envelope) error {
	if len(e.Results) == 0 {
		return nil
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	terminal, ok := out.(interface{ Fd() uintptr })
	color := ok && !noColor && os.Getenv("TERM") != "dumb" && term.IsTerminal(terminal.Fd())
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return '�'
			}
			return r
		}, s)
	}
	type row struct {
		service, symbol string
		urls            upURLs
		restart         bool
	}
	rows := make([]row, 0, len(e.Results))
	serviceWidth, localWidth := len("SERVICE"), len("LOCALHOST")
	for _, raw := range e.Results {
		item, _ := raw.(map[string]any)
		id, _ := item["instance_id"].(string)
		status, _ := item["status"].(string)
		restart, _ := item["restart_required"].(bool)
		urls := e.upURLs[id]
		urls.local, urls.domain, urls.reported = clean(urls.local), clean(urls.domain), clean(urls.reported)
		service := clean(serviceName(id))
		serviceWidth = max(serviceWidth, utf8.RuneCountInString(service))
		localWidth = max(localWidth, utf8.RuneCountInString(urls.local))
		rows = append(rows, row{service, upSymbol(status, color), urls, restart})
	}
	var table strings.Builder
	fmt.Fprintf(&table, "  %-*s  %-*s  CONNECTED DOMAIN\n", serviceWidth, "SERVICE", localWidth, "LOCALHOST")
	for _, row := range rows {
		line := fmt.Sprintf("%s %-*s  %-*s  %s", row.symbol, serviceWidth, row.service, localWidth, row.urls.local, row.urls.domain)
		table.WriteString(strings.TrimRight(line, " ") + "\n")
		if row.urls.reported != "" {
			fmt.Fprintf(&table, "  App URL  %s\n", row.urls.reported)
		}
		if row.restart {
			fmt.Fprintf(&table, "  Restart required. Run rdr restart %s to apply changes.\n", row.service)
		}
	}
	_, err := io.WriteString(out, table.String())
	return err
}
