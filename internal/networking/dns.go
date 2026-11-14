package networking

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Check every authoritative server. Recursive negative caching can otherwise
// hide a newly created challenge for longer than the propagation timeout.
func lookupAuthoritativeTXT(ctx context.Context, name string, servers []string) ([]string, error) {
	var common map[string]bool
	for _, server := range servers {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, server)
		}}
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		values, err := resolver.LookupTXT(queryCtx, strings.TrimSuffix(name, ".")+".")
		cancel()
		if err != nil {
			return nil, fmt.Errorf("authoritative TXT lookup at %s: %w", server, err)
		}
		present := make(map[string]bool, len(values))
		for _, value := range values {
			present[value] = true
		}
		if common == nil {
			common = present
		} else {
			for value := range common {
				if !present[value] {
					delete(common, value)
				}
			}
		}
	}
	values := make([]string, 0, len(common))
	for value := range common {
		values = append(values, value)
	}
	sort.Strings(values)
	return values, nil
}
