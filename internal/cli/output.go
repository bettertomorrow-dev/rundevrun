// Formats command results and errors as text or JSON.
package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/bettertomorrow-dev/rundevrun/internal/publication"
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
	return envelope{Version: 1, Command: command, Scope: scope{opts.project, opts.worktree},
		Status: "ok", DryRun: opts.dryRun, Results: []any{}, Errors: []commandError{}}
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
		for _, item := range e.Results {
			if _, err := fmt.Fprintln(out, item); err != nil {
				fmt.Fprintln(errOut, "rdr: cannot write result:", err)
				return 4
			}
		}
		for _, err := range e.Errors {
			fmt.Fprintf(errOut, "rdr: %s: %s\n", err.Code, err.Message)
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
