//go:build linux

// Tests runner preparation behavior on Linux.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestPublicCLIRecoversKilledBuild(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "rdr")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); err != nil {
		t.Fatalf("build rdr: %v: %s", err, output)
	}
	marker := filepath.Join(t.TempDir(), "forbidden")
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	boot := exec.Command(binary, "__rdr_bootstrap")
	boot.ExtraFiles = []*os.File{read}
	boot.Env = append(os.Environ(), "__RDR_COMMAND=touch "+marker)
	if err = boot.Start(); err != nil {
		t.Fatal(err)
	}
	read.Close()
	write.Close()
	if err = boot.Wait(); err == nil {
		t.Fatal("bootstrap accepted EOF")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("bootstrap executed before ACK: %v", err)
	}
	for _, tc := range []struct {
		restart bool
		recover string
	}{{false, "down"}, {true, "down"}, {false, "up"}, {true, "restart"}} {
		restart := tc.restart
		t.Run(fmt.Sprintf("restart=%v/recover=%s", restart, tc.recover), func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "runtime")
			configRoot := filepath.Join(dir, "config")
			if err := os.MkdirAll(filepath.Join(configRoot, "rundevrun"), 0700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			networking := fmt.Sprintf(`{"version":1,"local":{"listen":%q},"tailnet":{}}`, listener.Addr().String())
			listener.Close()
			if err := os.WriteFile(filepath.Join(configRoot, "rundevrun", "networking.json"), []byte(networking), 0600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(dir, "dev.toml")
			writeConfig := func(build bool) {
				text := "version = 1\n[project]\nname = 'demo'\n[services.worker]\ncommand = 'python3 -m http.server {port} --bind 127.0.0.1'\nport = 'auto'\nstop_timeout = '1s'\nbuild_timeout = '30s'\n"
				if build {
					text += "build = 'sleep 60 & echo $! > child.pid; echo ready > marker; wait'\n"
				}
				if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeConfig(false)
			cli := func(args ...string) *exec.Cmd {
				c := exec.Command(binary, args...)
				c.Dir = dir
				c.Env = append(os.Environ(), "RDR_HOME="+root, "XDG_CONFIG_HOME="+configRoot)
				return c
			}
			t.Cleanup(func() { _ = cli("down", "--all").Run(); _ = cli("proxy", "stop").Run() })
			if out, err := cli("register", ".", "--json").CombinedOutput(); err != nil {
				t.Fatalf("register: %v %s", err, out)
			}
			if restart {
				if out, err := cli("up", "worker", "--json").CombinedOutput(); err != nil {
					t.Fatalf("initial up: %v %s", err, out)
				}
			}
			p, err := discovery.Discover(dir)
			if err != nil {
				t.Fatal(err)
			}
			w := p.Worktrees[0]
			id := p.ID + "/" + w.ID + "/worker"
			store := &state.Store{Root: root}
			var oldRun string
			if restart {
				reg, err := store.Read()
				if err != nil {
					t.Fatal(err)
				}
				saved, err := reg.Project(p.ID)
				if err != nil {
					t.Fatal(err)
				}
				oldRun = saved.Instances[id].RunID
			}
			writeConfig(true)
			action := "up"
			if restart {
				action = "restart"
			}
			process := cli(action, "worker", "--json")
			var output bytes.Buffer
			process.Stdout = &output
			process.Stderr = &output
			if err = process.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { process.Process.Kill(); process.Wait(); cli("down", "worker", "--json").Run() }()
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if _, err = os.Stat(filepath.Join(dir, "marker")); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err != nil {
				process.Process.Kill()
				process.Wait()
				t.Fatalf("build marker absent: %v, output %s", err, output.String())
			}
			data, err := os.ReadFile(filepath.Join(dir, "child.pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cli("status", "--json").CombinedOutput(); err != nil {
				t.Fatalf("status during build: %v %s", err, out)
			}
			if out, err := cli("down", "worker", "--json").CombinedOutput(); err == nil || !bytes.Contains(out, []byte("BUSY")) {
				t.Fatalf("active preparation was not busy: %v %s", err, out)
			}
			ref, err := procid.Observe(pid)
			if err != nil {
				t.Fatal(err)
			}
			if err = process.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			process.Wait()
			reg, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			saved, err := reg.Project(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			i := saved.Instances[id]
			if i.Preparation == nil {
				t.Fatal("missing durable preparation")
			}
			preparationID := i.Preparation.RunID
			if i.Reservations[i.Preparation.RunID] == 0 {
				t.Fatal("missing build reservation")
			}
			if restart && i.RunID != oldRun {
				t.Fatal("old run replaced during build")
			}
			if restart {
				old, err := store.ReadRun(oldRun)
				if err != nil {
					t.Fatal(err)
				}
				if live, err := procid.Check(old.Child); err != nil || !live {
					t.Fatalf("old process died: %v %v", live, err)
				}
			}
			if tc.recover != "down" {
				writeConfig(false)
			}
			if out, err := cli(tc.recover, "worker", "--json").CombinedOutput(); err != nil {
				t.Fatalf("recovery %s: %v %s", tc.recover, err, out)
			}
			if live, err := procid.Check(ref); err != nil || live {
				t.Fatalf("build child remains: %v %v", live, err)
			}
			reg, err = store.Read()
			if err != nil {
				t.Fatal(err)
			}
			saved, err = reg.Project(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			i = saved.Instances[id]
			if i.Preparation != nil || i.Reservations[preparationID] != 0 {
				t.Fatalf("preparation/reservation not cleared: %+v", i)
			}
			if tc.recover == "down" && len(i.Reservations) != 0 {
				t.Fatalf("down retained reservation: %+v", i)
			}
		})
	}
}

func TestPreparationForeignIdentityIsNotSignalledOrCleared(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/s"
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: {ID: id, Service: "s", WorktreeID: "w", State: "stopped", Preparation: &state.Intent{RunID: "prep", OperationID: "op", Phase: "preparing"}, Reservations: map[string]int{"prep": 12345}}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command("/bin/sleep", "60")
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	ref, err := procid.Observe(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	foreign := ref
	foreign.Identity += "foreign"
	if err = store.WriteManifest("prep", state.Manifest{RunID: "prep", InstanceID: id, OperationID: "op"}); err != nil {
		t.Fatal(err)
	}
	if err = store.WritePreparation("prep", state.PreparationRecord{Child: foreign}); err != nil {
		t.Fatal(err)
	}
	_, err = New(store, "").Stop(context.Background(), &discovery.Project{ID: "p"}, &discovery.Worktree{ID: "w", Root: dir}, "s")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("expected unknown ownership: %v", err)
	}
	if live, err := procid.Check(ref); err != nil || !live {
		t.Fatalf("foreign process harmed: %v %v", live, err)
	}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Project("p")
	if err != nil {
		t.Fatal(err)
	}
	if p.Instances[id].Preparation == nil || p.Instances[id].Reservations["prep"] != 12345 {
		t.Fatal("unresolved preparation cleared")
	}
}
