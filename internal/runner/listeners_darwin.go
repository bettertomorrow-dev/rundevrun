//go:build darwin

package runner

import (
	"errors"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func checkListeners(_ []procid.Ref, _ int) error {
	return errors.New("listener ownership inspection unavailable on macOS")
}
