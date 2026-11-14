package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestGraphErrorCode(t *testing.T) {
	for _, v := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("wrapped: %w", state.ErrCorrupt), "STATE_CORRUPT"}, {state.ErrBusy, "BUSY"},
		{discovery.ErrAmbiguous, "AMBIGUOUS"}, {discovery.ErrNotFound, "NOT_FOUND"},
		{config.ErrVersionUnsupported, "VERSION_UNSUPPORTED"}, {&config.Error{Code: "CONFIG_INVALID"}, "CONFIG_INVALID"},
		{&Error{"PORT_CONFLICT", fmt.Errorf("conflict")}, "PORT_CONFLICT"}, {context.Canceled, "CANCELLED"},
		{&Error{"READINESS_TIMEOUT", context.DeadlineExceeded}, "READINESS_TIMEOUT"},
		{&Error{"BUILD_FAILED", context.DeadlineExceeded}, "BUILD_FAILED"},
		{errors.Join(&Error{"IO_ERROR", fmt.Errorf("cleanup")}, context.Canceled), "CANCELLED"},
		{fmt.Errorf("disk failure"), "IO_ERROR"},
	} {
		if got := graphErrorCode(v.err); got != v.code {
			t.Errorf("%v: %s want %s", v.err, got, v.code)
		}
	}
}

func TestInvalidGraphSelectionHasErrorStatus(t *testing.T) {
	r := New(&state.Store{Root: t.TempDir()}, "")
	for _, execute := range []func(context.Context, []Selection) (GraphResult, error){r.UpGraph, r.RestartGraph} {
		result, err := execute(context.Background(), []Selection{{}})
		if err == nil || result.Status != "error" || len(result.Errors) == 0 || len(result.Results) != 0 {
			t.Fatalf("invalid selection: %+v, %v", result, err)
		}
	}
}
