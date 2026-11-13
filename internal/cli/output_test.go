package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/publication"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
)

func TestJSONResultIsOneDocumentWithArrays(t *testing.T) {
	e := result("services", options{json: true})
	e.fail("AMBIGUOUS", "select a worktree", "use --worktree with an ID")
	var out, errOut bytes.Buffer
	if code := writeResult(&out, &errOut, e, true); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	dec := json.NewDecoder(&out)
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != float64(1) || got["status"] != "error" || got["results"] == nil || got["errors"] == nil {
		t.Fatalf("invalid envelope: %#v", got)
	}
	if err := dec.Decode(&got); err != io.EOF {
		t.Fatalf("stdout has trailing data: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatal(errOut.String())
	}
}

func TestInterspersedSelectorsAndInvalidFlags(t *testing.T) {
	opts, args, err := parseArgs([]string{"--json", "services", "--project=p_1", "--worktree", "/tmp/a b"})
	if err != nil || !opts.json || opts.project != "p_1" || opts.worktree != "/tmp/a b" || len(args) != 1 || args[0] != "services" {
		t.Fatalf("options=%+v args=%v error=%v", opts, args, err)
	}
	for _, args := range [][]string{{"--worktree"}, {"--project="}, {"--json=false"}, {"--json", "--json"}, {"--unknown"}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestLogOptions(t *testing.T) {
	opts, args, err := parseArgs([]string{"logs", "worker", "--tail=25", "--follow"})
	if err != nil || !opts.follow || !opts.tailSet || opts.tail != 25 || len(args) != 2 {
		t.Fatalf("options=%+v args=%v error=%v", opts, args, err)
	}
	for _, args := range [][]string{{"--tail"}, {"--tail=bad"}, {"--tail=-1"}, {"--tail=10001"}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestCancellationExitCodeWinsOverPartialAndEarlierErrors(t *testing.T) {
	e := result("down", options{json: true})
	e.Status = "error"
	e.Errors = append(e.Errors,
		commandError{Code: "IO_ERROR", Message: "cleanup failed", Actions: []string{}},
		commandError{Code: "CANCELLED", Message: "context canceled", Actions: []string{}},
	)
	var out, errOut bytes.Buffer
	if code := writeResult(&out, &errOut, e, true); code != 130 {
		t.Fatalf("exit = %d, want cancellation code 130", code)
	}
}

func TestSharePasswordIsOneTimeCredentialOnlyInJSON(t *testing.T) {
	e := result("share", options{json: true})
	e.Results = append(e.Results, publicationResult{Status: publication.Status{State: "ready", URL: "https://fixture.trycloudflare.com"}, Password: "one-time-secret"})
	e.Credentials = &shareCredentials{Password: "one-time-secret"}
	var out, errOut bytes.Buffer
	if code := writeResult(&out, &errOut, e, true); code != 0 {
		t.Fatalf("share exit=%d: %s", code, errOut.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["credentials"].(map[string]any)["password"] != "one-time-secret" {
		t.Fatalf("missing one-time credential: %#v", got)
	}
	resultJSON, err := json.Marshal(got["results"])
	if err != nil || bytes.Contains(resultJSON, []byte("one-time-secret")) {
		t.Fatalf("password leaked into result item: %s (%v)", resultJSON, err)
	}
	e.Credentials = nil
	e.Results = []any{publicationResult{Status: publication.Status{State: "ready", URL: "https://fixture.trycloudflare.com"}}}
	out.Reset()
	if code := writeResult(&out, &errOut, e, false); code != 0 || bytes.Contains(out.Bytes(), []byte("Password:")) {
		t.Fatalf("repeat share exposed password: code=%d stdout=%q", code, out.String())
	}
}

func TestShareCleanupErrorPreservesActivePublicationWithoutCredentials(t *testing.T) {
	e := result("share", options{json: true})
	e.Credentials = &shareCredentials{Password: "previous-value-must-be-cleared"}
	failShare(&e, publication.Result{
		Status:   publication.Status{ID: "new-share", State: "ready", URL: "https://fixture.trycloudflare.com"},
		Password: "must-not-escape-on-error",
	}, &runner.Error{Code: "TUNNEL_FAILED", Err: errors.New("old publication cleanup failed")})
	var out, errOut bytes.Buffer
	if code := writeResult(&out, &errOut, e, true); code != 5 || e.Status != "partial" || len(e.Results) != 1 || e.Credentials != nil {
		t.Fatalf("share partial result: code=%d envelope=%+v", code, e)
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &encoded); err != nil {
		t.Fatal(err)
	}
	_, exposesCredentials := encoded["credentials"]
	if bytes.Contains(out.Bytes(), []byte("must-not-escape-on-error")) || exposesCredentials {
		t.Fatalf("share error exposed credentials: %s", out.String())
	}
}

func TestSharePreparationErrorPreservesFailedStatusAsError(t *testing.T) {
	e := result("share", options{json: true})
	failShare(&e, publication.Result{Status: publication.Status{ID: "failed-share", State: "failed", URL: "https://fixture.trycloudflare.com"}, LogPaths: []string{"runs/share/tunnel.log"}},
		&runner.Error{Code: "BUILD_FAILED", Err: errors.New("snapshot build failed")})
	var out, errOut bytes.Buffer
	if code := writeResult(&out, &errOut, e, true); code != 4 || e.Status != "error" || len(e.Results) != 1 || e.Credentials != nil {
		t.Fatalf("share preparation failure: code=%d envelope=%+v", code, e)
	}
	item, ok := e.Results[0].(publicationResult)
	if !ok || len(item.LogPaths) != 1 || item.LogPaths[0] != "runs/share/tunnel.log" || !bytes.Contains([]byte(item.String()), []byte("runs/share/tunnel.log")) {
		t.Fatalf("failed share omitted safe log paths: %#v", e.Results)
	}
}
