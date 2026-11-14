package runner

import (
	"os/exec"
	"testing"
)

func TestSubstitutionKeepsShellValueLiteral(t *testing.T) {
	value := "space ' $HOME $(exit 1)\nend"
	cmd, env, err := substitute(`printf '%s' {run_dir}`, map[string]string{"run_dir": value}, true)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("/bin/sh", "-c", cmd)
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	got, err := c.Output()
	if err != nil || string(got) != value {
		t.Fatalf("got %q: %v", got, err)
	}
	raw, _, err := substitute(`{{hello}}-{run_dir}`, map[string]string{"run_dir": value}, false)
	if err != nil || raw != "{hello}-"+value {
		t.Fatalf("got %q: %v", raw, err)
	}
	if _, _, err := substitute("{port}", nil, false); err == nil {
		t.Fatal("unavailable placeholder accepted")
	}
}

func TestOrdinaryShellPreserved(t *testing.T) {
	for _, input := range []string{"f() { echo hi; }; f", "echo ${HOME:-x}"} {
		got, _, err := substitute(input, nil, true)
		if err != nil || got != input {
			t.Fatalf("%q became %q: %v", input, got, err)
		}
	}
	got, _, err := substitute("echo {{literal}}", nil, true)
	if err != nil || got != "echo {literal}" {
		t.Fatalf("got %q: %v", got, err)
	}
}

func TestUnavailableCommandPlaceholder(t *testing.T) {
	if _, _, err := substitute("echo {port}", nil, true); err == nil {
		t.Fatal("unavailable placeholder accepted")
	}
}
