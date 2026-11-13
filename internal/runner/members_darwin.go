//go:build darwin

package runner

import (
	"errors"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func groupMembers(_ procid.Ref, _ []procid.Ref) ([]procid.Ref, error) {
	return nil, errors.New("process group enumeration unavailable on macOS")
}
