// Identifies listeners that a proxy state directory cannot control.
package proxy

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
)

func unrecognizedListener(ctx context.Context, configPath string) (Info, *Error) {
	if configPath == "" {
		var err error
		configPath, err = routing.DefaultPath()
		if err != nil {
			return Info{}, nil
		}
	}
	cfg, err := localrouting.Load(configPath)
	if err != nil {
		return Info{}, nil
	}
	address := cfg.Routing.HTTPListen
	dialer := net.Dialer{Timeout: 150 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return Info{}, nil
	}
	conn.Close()
	pid, command, inspect := listenerOwner(ctx, address)
	message := fmt.Sprintf("unrecognized listener on %s", address)
	actions := []string{inspect}
	if pid > 0 {
		message += fmt.Sprintf(" (PID %d", pid)
		if command != "" {
			message += ", " + command
		}
		message += ")"
		actions = append(actions, fmt.Sprintf("ps -p %d -o command=", pid))
		if command == "rundevrun" {
			actions = append(actions, fmt.Sprintf("If the command confirms this is the stale __rdr_proxy process, run kill -TERM %d, then rerun rundevrun up.", pid))
		}
	}
	actions = append(actions, "Stop the process that owns the listener or change the local proxy listen address, then rerun rundevrun up.")
	return Info{State: "stale", Identity: procid.Ref{PID: pid}, ConfigPath: configPath, HTTPListen: address}, &Error{Code: "STALE_PROXY", Message: message, Recovery: actions}
}

var ssOwner = regexp.MustCompile(`\("([^"]+)",pid=([0-9]+),`)

func listenerOwner(ctx context.Context, address string) (pid int, command, inspect string) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return 0, "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if runtime.GOOS == "darwin" {
		filter := "-iTCP:" + port
		inspect = "lsof -nP " + filter + " -sTCP:LISTEN"
		out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", filter, "-sTCP:LISTEN", "-Fpcn").Output()
		if err != nil {
			return 0, "", inspect
		}
		for _, line := range strings.Split(string(out), "\n") {
			switch {
			case strings.HasPrefix(line, "p"):
				pid, _ = strconv.Atoi(line[1:])
				command = ""
			case strings.HasPrefix(line, "c"):
				command = line[1:]
			case strings.HasPrefix(line, "n"):
				if listenerAddressMatches(line[1:], host, port) {
					return pid, command, inspect
				}
			}
		}
		return 0, "", inspect
	}
	filter := "sport = :" + port
	inspect = "ss -Hltnp '" + filter + "'"
	out, err := exec.CommandContext(ctx, "ss", "-Hltnp", filter).Output()
	if err != nil {
		return 0, "", inspect
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !listenerAddressMatches(fields[3], host, port) {
			continue
		}
		if match := ssOwner.FindStringSubmatch(line); match != nil {
			pid, _ = strconv.Atoi(match[2])
			return pid, match[1], inspect
		}
	}
	return 0, "", inspect
}

func listenerAddressMatches(address, host, port string) bool {
	listenerHost, listenerPort, err := net.SplitHostPort(address)
	if err != nil || listenerPort != port {
		return false
	}
	ip := net.ParseIP(listenerHost)
	return listenerHost == "*" || ip.IsUnspecified() || listenerHost == host || ip.Equal(net.ParseIP(host))
}
