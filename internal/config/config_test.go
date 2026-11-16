// Tests config behavior.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "dev.toml")
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}

const base = `version = 1
[project]
name = "demo"
[services.api]
command = "echo {port}"
port = "auto"
protocol = "http"
`

func TestRoutingDomainValidation(t *testing.T) {
	for _, domain := range []string{"-bad.example.com", "bad-.example.com", "bad_name.example.com", ".example.com", "example.com.", strings.Repeat("x", 64) + ".example.com", "bad\n.example.com"} {
		body := base + "\n[routing]\ndomain=" + fmt.Sprintf("%q", domain) + "\n"
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Fatalf("accepted domain %q", domain)
		}
	}
	if _, err := Load(writeConfig(t, base+"\n[routing]\ndomain=\"dev.example.com\"\n")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAndValidation(t *testing.T) {
	c, e := Load(writeConfig(t, base))
	if e != nil {
		t.Fatal(e)
	}
	if c.Project.Name != "demo" || c.Digest == "" {
		t.Fatalf("bad descriptor: %#v", c)
	}
	for _, body := range []string{strings.Replace(base, "version = 1", "version = 2", 1), strings.Replace(base, "command = \"echo {port}\"", "command = \"echo '{port}'\"", 1), base + "\n[services.api.extra]\nx=1\n"} {
		if _, e := Load(writeConfig(t, body)); e == nil {
			t.Fatalf("accepted invalid config: %s", body)
		}
	}
}
func TestConfigConfinementAndNormalizedDependencyCollision(t *testing.T) {
	d := t.TempDir()
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(d, "escape")); e != nil {
		t.Fatal(e)
	}
	body := `version=1
[project]
name="demo"
[services.api-v1]
command="run"
[services.api_v1]
command="run"
[services.consumer]
command="run"
depends_on=["api-v1","api_v1"]
`
	p := filepath.Join(d, "dev.toml")
	os.WriteFile(p, []byte(body), 0600)
	if _, e := Load(p); e == nil || !strings.Contains(e.Error(), "normalization") {
		t.Fatalf("collision: %v", e)
	}
	body = strings.Replace(body, "depends_on=[\"api-v1\",\"api_v1\"]", "", 1) + "cwd=\"escape\"\n"
	os.WriteFile(p, []byte(body), 0600)
	if _, e := Load(p); e == nil {
		t.Fatal("accepted symlink escape")
	}
}
func TestShellVariablesAndQuotedPlaceholder(t *testing.T) {
	c := &Config{Services: map[string]Service{"api": {Protocol: "http"}}}
	if e := ValidateCommand(`echo {{literal}}`, c); e != nil {
		t.Fatal(e)
	}
	if e := ValidateCommand(`echo ${HOME} {port}`, c); e != nil {
		t.Fatal(e)
	}
	if e := ValidateCommand(`echo '{port}'`, c); e == nil {
		t.Fatal("quoted placeholder accepted")
	}
	if e := ValidateEnv(`{{x}} ${HOME}`, c); e != nil {
		t.Fatal(e)
	}
}

func TestLoadDoesNotExecuteCommands(t *testing.T) {
	d := t.TempDir()
	marker := filepath.Join(d, "ran")
	body := "version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"touch " + marker + "\"\n"
	if _, e := Load(writeConfig(t, body)); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("config load executed project command")
	}
}

func TestFullTechSpecExampleAndDependencyPlaceholders(t *testing.T) {
	d := t.TempDir()
	if e := os.MkdirAll(filepath.Join(d, "apps", "console"), 0755); e != nil {
		t.Fatal(e)
	}
	body := `version=1
[project]
name="example"
[services.api]
cwd="."
build="go build -o {run_dir}/api ./cmd/api"
command="{run_dir}/api --addr 127.0.0.1:{port}"
protocol="http"
port="auto"
depends_on=[]
build_timeout="5m"
start_timeout="30s"
stop_timeout="20s"
health={http="/health",timeout="30s"}
env={HOST="{host}",PORT="{port}"}
hooks={before_stop="./scripts/drain",after_stop="./scripts/backup"}
hook_timeout="60s"
[services.console]
cwd="apps/console"
command="pnpm dev -- --host 127.0.0.1 --port {port} --strictPort"
protocol="http"
port="auto"
depends_on=["api"]
env={VITE_API_URL="{dependencies.api.url}"}
routes=[{path="/api",dependency="api",strip_prefix=false}]
[services.console.share]
mode="snapshot"
auth="password"
build="pnpm build"
dir="dist"
[services.console.share.env]
VITE_API_URL="/api"
[services.worker]
command="./scripts/worker"
depends_on=["api"]
env={API_URL="{dependencies.api.direct_url}"}
`
	p := filepath.Join(d, "dev.toml")
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if c.Services["console"].Protocol != "http" {
		t.Fatal("default protocol not applied before routes")
	}
}
func TestPlaceholdersRequireDeclaredAvailableDependencies(t *testing.T) {
	d := t.TempDir()
	body := `version=1
[project]
name="demo"
[services.api]
command="run"
protocol="http"
port="auto"
[services.worker]
command="run"
depends_on=["api"]
env={API_URL="{dependencies.api.direct_url}"}
`
	if _, e := Load(writeConfig(t, body)); e != nil {
		t.Fatal(e)
	}
	bad := strings.Replace(body, "depends_on=[\"api\"]", "depends_on=[]", 1)
	if _, e := Load(writeConfig(t, bad)); e == nil {
		t.Fatal("accepted undeclared dependency")
	}
	_ = d
}
func TestHooksAndEmptyOptionalFields(t *testing.T) {
	cases := []string{
		strings.Replace(base, "command = \"echo {port}\"", "command = \"echo {port}\"\nbuild = \" \"", 1),
		strings.Replace(base, "command = \"echo {port}\"", "command = \"echo {port}\"\n[services.api.share]\nmode=\"live\"\nbuild=\"\"", 1),
		strings.Replace(base, "command = \"echo {port}\"", "command = \"echo {port}\"\n[services.api.hooks]\nbefore_start = \"echo '{port}'\"", 1),
		strings.Replace(base, "command = \"echo {port}\"", "command = \"echo {port}\"\ncwd = \"\"", 1),
		strings.Replace(base, "command = \"echo {port}\"", "command = \"echo {port}\"\nstart_timeout = \"\"", 1),
	}
	for i, b := range cases {
		if _, e := Load(writeConfig(t, b)); e == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
func TestCWDMustBeDirectory(t *testing.T) {
	d := t.TempDir()
	if e := os.WriteFile(filepath.Join(d, "file"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	body := strings.Replace(base, "command = \"echo {port}\"", "command = \"echo {port}\"\ncwd = \"file\"", 1)
	if _, e := Load(writeConfig(t, body)); e == nil {
		t.Fatal("accepted file as cwd")
	}
}

func TestRouteRequiresDeclaredHTTPDependencyRegardlessOfSortOrder(t *testing.T) {
	body := `version=1
[project]
name="demo"
[services.consumer]
command="run"
protocol="http"
port="auto"
depends_on=["z-api"]
routes=[{path="/api",dependency="z-api"}]
[services.z-api]
command="run"
port="auto"
`
	if _, e := Load(writeConfig(t, body)); e != nil {
		t.Fatalf("later defaulted HTTP dependency rejected: %v", e)
	}
	body = strings.Replace(body, "depends_on=[\"z-api\"]", "depends_on=[]", 1)
	if _, e := Load(writeConfig(t, body)); e == nil {
		t.Fatal("accepted route to undeclared dependency")
	}
}

func TestLoadErrorsExposeCLIFields(t *testing.T) {
	for _, body := range []string{"version = [", strings.Replace(base, "command = \"echo {port}\"", "command = \"echo {port}\"\nbuild = \"\"", 1)} {
		_, err := Load(writeConfig(t, body))
		var ce *Error
		if !errors.As(err, &ce) || ce.Code != "CONFIG_INVALID" || ce.Field == "" {
			t.Fatalf("not CLI typed: %#v %v", ce, err)
		}
	}
}
func TestHostPlaceholderDoesNotRequirePort(t *testing.T) {
	body := `version=1
[project]
name="demo"
[services.worker]
command="run {host}"
`
	if _, e := Load(writeConfig(t, body)); e != nil {
		t.Fatal(e)
	}
}
