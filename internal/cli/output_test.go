package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
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
