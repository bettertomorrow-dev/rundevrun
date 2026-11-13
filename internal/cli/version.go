package cli

import "fmt"

// Version may be set at build time with -ldflags -X.
var Version = "0.1.0-dev"

// APIVersion identifies the public command, JSON and configuration contract.
const APIVersion = 1

type versionInfo struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	APIVersion int    `json:"api_version"`
}

func (v versionInfo) String() string {
	return fmt.Sprintf("%s %s API %d", v.Name, v.Version, v.APIVersion)
}
