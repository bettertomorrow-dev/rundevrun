package procid

import (
	"os"
	"testing"
	"time"
)

func TestIdentityMismatchNeverSignals(t *testing.T) {
	r, err := Observe(os.Getpid())
	if err != nil || !Matches(r) {
		t.Fatalf("%+v %v", r, err)
	}
	for _, mutate := range []func(*Ref){func(r *Ref) { r.Identity += "bad" }, func(r *Ref) { r.BootID += "bad" }, func(r *Ref) { r.PGID++ }, func(r *Ref) { r.SID++ }} {
		foreign := r
		mutate(&foreign)
		if Matches(foreign) {
			t.Fatal("foreign process matches")
		}
		if len(Terminate([]Ref{foreign}, time.Millisecond)) != 0 {
			t.Fatal("foreign process reported alive")
		}
		if !Matches(r) {
			t.Fatal("own process changed")
		}
	}
}
