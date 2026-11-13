package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func project(id string) Project {
	return Project{ID: id, Name: id, Root: "/tmp/" + id, Instances: map[string]Instance{}}
}
func TestConcurrentUpdatesAndCorruptionIsolation(t *testing.T) {
	s, _ := New(t.TempDir())
	if err := s.Update(func(r *Registry) error { r.Projects["broken"] = json.RawMessage(`{"id":42}`); return nil }); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprint(n)
			if err := s.Update(func(r *Registry) error { return r.SetProject(id, project(id)) }); err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	r, err := s.Read()
	if err != nil || len(r.Projects) != 21 || r.Revision != 21 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = r.Project("broken"); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if _, err = r.Project("0"); err != nil {
		t.Fatal(err)
	}
	var damaged map[string]int
	if json.Unmarshal(r.Projects["broken"], &damaged) != nil || damaged["id"] != 42 {
		t.Fatal(string(r.Projects["broken"]))
	}
}
func TestFailureDoesNotReplaceRegistry(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Update(func(r *Registry) error { return r.SetProject("a", project("a")) })
	before, _ := os.ReadFile(filepath.Join(s.Root, "registry.json"))
	if err := s.Update(func(r *Registry) error { return errors.New("interrupted") }); err == nil {
		t.Fatal("expected error")
	}
	after, _ := os.ReadFile(filepath.Join(s.Root, "registry.json"))
	if string(before) != string(after) {
		t.Fatal("registry changed")
	}
	os.WriteFile(filepath.Join(s.Root, "registry.json"), []byte("{"), 0600)
	if err := s.Update(func(*Registry) error { return nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}
func TestPrivateAndSymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(*Registry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "locks")} {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0700 {
			t.Fatal(p, st.Mode())
		}
	}
	st, _ := os.Stat(filepath.Join(root, "registry.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	os.Remove(filepath.Join(root, "registry.json"))
	os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "registry.json"))
	if _, err = s.Read(); err == nil {
		t.Fatal("symlink accepted")
	}
	if err = s.Update(func(*Registry) error { return nil }); err == nil {
		t.Fatal("symlink overwritten")
	}
}
func TestRecoveryBusy(t *testing.T) {
	s, _ := New(t.TempDir())
	l, err := s.LockInstances("p/w/a")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = s.Recover("p", "p/w/a"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}

func TestRegistrySubprocess(t *testing.T) {
	if root := os.Getenv("RDR_TEST_ROOT"); root != "" {
		s := &Store{Root: root}
		id := os.Getenv("RDR_TEST_ID")
		if err := s.Update(func(r *Registry) error { return r.SetProject(id, project(id)) }); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	var cmds []*exec.Cmd
	for n := 0; n < 12; n++ {
		c := exec.Command(os.Args[0], "-test.run=^TestRegistrySubprocess$")
		c.Env = append(os.Environ(), "RDR_TEST_ROOT="+root, fmt.Sprintf("RDR_TEST_ID=%d", n))
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	r, err := (&Store{Root: root}).Read()
	if err != nil || r.Revision != 12 || len(r.Projects) != 12 {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestOrphanTempDoesNotReplacePublishedRegistry(t *testing.T) {
	s, _ := New(t.TempDir())
	if err := s.Update(func(r *Registry) error { return r.SetProject("a", project("a")) }); err != nil {
		t.Fatal(err)
	}
	// Simulate a writer killed after creating and partially writing its temp file.
	f, err := os.CreateTemp(s.Root, ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"schema":1,"revision":`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r, err := s.Read()
	if err != nil || r.Revision != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = r.Project("a"); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(r *Registry) error { return r.SetProject("b", project("b")) }); err != nil {
		t.Fatal(err)
	}
	r, err = s.Read()
	if err != nil || r.Revision != 2 || len(r.Projects) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestNestedCorruptionAndUncertainRun(t *testing.T) {
	s, _ := New(t.TempDir())
	p := project("p")
	p.Worktrees = []Worktree{{ID: "w", Root: "/tmp/w"}}
	i := Instance{ID: "p/w/s", WorktreeID: "w", Service: "s", State: "running", RunID: "run"}
	p.Instances[i.ID] = i
	if err := s.Update(func(r *Registry) error { return r.SetProject("p", p) }); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Read()
	bad := p
	bad.Instances = map[string]Instance{"p/w/other": i}
	b, _ := json.Marshal(bad)
	r.Projects["p"] = b
	if _, err := r.Project("p"); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if err := s.WriteManifest("run", Manifest{RunID: "run", InstanceID: i.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteRun("run", RunRecord{Claim: "accepted", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inspect(i); err == nil {
		t.Fatal("uncertain claim reported stale")
	}
	if err := s.Recover("p", i.ID); err == nil {
		t.Fatal("uncertain claim recovered")
	}
	if err := s.WriteRun("run", RunRecord{State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover("p", i.ID); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Read()
	p, err := r.Project("p")
	if err != nil || p.Instances[i.ID].State != "stopped" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestInspectMissingSupervisorPhase(t *testing.T) {
	s, e := New(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	id := "p/w/s"
	if e = s.WriteManifest("run", Manifest{RunID: "run", InstanceID: id}); e != nil {
		t.Fatal(e)
	}
	if _, err := s.Inspect(Instance{ID: id, RunID: "run", State: "running"}); !errors.Is(err, ErrOwnershipUnknown) {
		t.Fatalf("missing intent should be uncertain: %v", err)
	}
	for _, tc := range []struct {
		name, state, phase, want string
		unknown                  bool
	}{{"unclaimed", "starting", "spawning", "starting", false}, {"revoked", "stopped", "stopped", "stopped", false}, {"started", "running", "started", "", true}, {"interrupted", "failed", "interrupted", "", true}, {"inconsistent", "starting", "stopped", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Inspect(Instance{ID: id, RunID: "run", State: tc.state, Intent: &Intent{RunID: "run", OperationID: "op", Phase: tc.phase}})
			if got != tc.want || errors.Is(err, ErrOwnershipUnknown) != tc.unknown || (!tc.unknown && err != nil) {
				t.Fatalf("got %q err %v", got, err)
			}
		})
	}
}
