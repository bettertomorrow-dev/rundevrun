// Package localrouting derives runtime routing settings without modifying files.
package localrouting

import (
	"context"
	"path/filepath"

	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
)

func Load(path string) (routing.Config, error) {
	cfg, err := routing.Load(path)
	if err != nil {
		return cfg, err
	}
	legacy := cfg
	hadLegacy := cfg.Routing.Enabled
	snap, err := (networking.Service{Root: filepath.Dir(path)}).Inspect(context.Background())
	if err != nil {
		return routing.Config{}, err
	}
	if hadLegacy {
		cfg.Legacy = &legacy
	}
	return WithDesired(cfg, snap.Config), nil
}

func WithDesired(cfg routing.Config, desired networking.Config) routing.Config {
	cfg.LocalOnly = true
	cfg.TailnetDomain = desired.Tailnet.Domain
	cfg.LocalProjects = make(map[string]routing.LocalProject, len(desired.Projects))
	for id, project := range desired.Projects {
		cfg.LocalProjects[id] = routing.LocalProject{DefaultService: project.DefaultService, PrimaryWorktree: project.PrimaryWorktree, Tailnet: project.Tailnet}
	}
	cfg.Routing.Enabled = true
	cfg.Routing.Domain = "localhost"
	cfg.Routing.Hostname = "{service}-{worktree}-{project}.{domain}"
	cfg.Routing.HTTPListen = desired.Local.Listen
	cfg.Routing.HTTPSListen = networking.ManagedTLSTarget(cfg.Legacy)
	cfg.Routing.TLS = routing.TLSSettings{}
	cfg.Routing.TrustedProxies = nil
	cfg.Routing.ExternalHTTPSPort = nil
	return cfg
}
