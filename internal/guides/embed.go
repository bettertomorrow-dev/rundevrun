// Package guides loads bundled, downloaded, and local guide directories.
package guides

import "embed"

//go:embed manifest.json onboard domain
var bundled embed.FS
