// Tests proxy export behavior.
package proxy

import (
	"context"
	"crypto/tls"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func SetCertificateLoaderForTest(loader func(networking.Service, string) (*tls.Certificate, error)) func() {
	previous := loadManagedCertificate
	loadManagedCertificate = loader
	return func() { loadManagedCertificate = previous }
}
func StopInProcessForTest(ctx context.Context, store *state.Store) error {
	_, err := control(ctx, store, "stop")
	return err
}
