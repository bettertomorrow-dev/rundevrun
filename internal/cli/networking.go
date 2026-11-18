// networkingCommand handles the settings-only network CLI. Provider checks and external reconciliation are supplied by the networking backend.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/networkactivation"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func networkingCommand(ctx context.Context, store *state.Store, args []string, opts options, e *envelope) {
	if len(args) < 2 || len(args) > 4 || (len(args) > 2 && args[1] != "credential-store" && args[1] != "credential-replace" && args[1] != "adopt-dns" && args[1] != "adopt-existing-dns" && args[1] != "adopt-serve" && args[1] != "resolve-challenge") {
		e.fail("USAGE", "usage: rundevrun network inspect|check|plan|apply|setup-check|setup-plan|setup-apply|credential-store")
		return
	}
	command := args[1]
	path := opts.file
	service, err := networking.DefaultService()
	if err != nil {
		failError(e, err)
		return
	}
	switch command {
	case "inspect":
		if path != "" || opts.dryRun {
			e.fail("USAGE", "network inspect does not accept --file or --dry-run")
			return
		}
		snapshot, err := service.Inspect(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		e.Results = append(e.Results, snapshot)
	case "check", "setup-check":
		if path != "" || opts.dryRun {
			e.fail("USAGE", fmt.Sprintf("network %s does not accept --file or --dry-run", command))
			return
		}
		status, err := service.Check(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		e.Results = append(e.Results, status)
	case "plan":
		if opts.dryRun {
			e.fail("USAGE", "network plan is always read-only; omit --dry-run")
			return
		}
		snapshot, err := service.Inspect(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		desired := snapshot.Config
		if path != "" {
			if err := readNetworkJSON(path, &desired); err != nil {
				failError(e, err)
				return
			}
		}
		plan, err := service.PlanAtRevision(ctx, desired, snapshot.Revision)
		if err != nil {
			failError(e, err)
			return
		}
		cfg, _, err := machineRouting()
		if err != nil {
			failError(e, err)
			return
		}
		routes, err := runner.PlanLocalRoutes(ctx, store, localrouting.WithDesired(cfg, desired))
		if err != nil {
			failError(e, err)
			return
		}
		e.Plan = networkPlanFile{Settings: plan, RouteRevision: routes.Revision, RouteChanges: routes.Changes}
	case "apply":
		if opts.dryRun || path == "" {
			e.fail("USAGE", "network apply requires --file PLAN.json and does not accept --dry-run")
			return
		}
		var reviewed networkPlanFile
		if err := readNetworkJSON(path, &reviewed); err != nil {
			failError(e, err)
			return
		}
		plan := reviewed.Settings
		before, err := service.Inspect(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		_, configPath, err := machineRouting()
		if err != nil {
			failError(e, err)
			return
		}
		activation, err := networkactivation.Prepare(ctx, store, executablePath(), configPath, plan.Desired)
		if err != nil {
			failError(e, err)
			return
		}
		defer activation.Abort()
		if activation.RouteRevision() != reviewed.RouteRevision || !slices.Equal(activation.RouteChanges(), reviewed.RouteChanges) {
			failError(e, errors.New("route candidates changed after preview; create a new network plan"))
			return
		}
		snapshot, err := service.Apply(ctx, plan)
		if err != nil {
			if errors.Is(err, networking.ErrConflict) {
				e.fail("STATE_CONFLICT", "network settings changed after this plan; create a new plan")
			} else {
				failError(e, err)
			}
			return
		}
		e.Results = append(e.Results, snapshot)
		if err := activation.Commit(ctx); err != nil {
			rollbackErr := rollbackNetworkSettings(ctx, service, before.Config, snapshot.Revision)
			if rollbackErr == nil {
				e.Results = e.Results[:len(e.Results)-1]
				e.fail("ROUTE_CONFLICT", "registry changed before route commit; networking settings were rolled back", "inspect `rundevrun network check --json` and create a fresh plan")
				return
			}
			e.Status = "partial"
			message := "route plan could not be committed after settings were saved: " + err.Error() + "; settings rollback also failed: " + rollbackErr.Error()
			e.Errors = append(e.Errors, commandError{Code: "ROUTE_RECONCILE_FAILED", Message: message, Actions: []string{"run `rundevrun network inspect --json`, then create and apply a fresh plan"}})
		} else {
			if err := activation.Finish(ctx); err != nil {
				e.Status = "partial"
				e.Errors = append(e.Errors, commandError{Code: "ROUTE_RECONCILE_FAILED", Message: "network settings and routes were saved, but proxy reload failed: " + err.Error(), Actions: []string{"run `rundevrun network check --json`, then `rundevrun proxy reload`"}})
			}
		}
	case "setup-plan":
		if path != "" || opts.dryRun {
			e.fail("USAGE", "network setup-plan does not accept --file or --dry-run")
			return
		}
		plan, err := service.PlanSetup(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		e.Plan = plan
	case "cleanup-plan":
		if path != "" || opts.dryRun {
			e.fail("USAGE", "network cleanup-plan does not accept --file or --dry-run")
			return
		}
		plan, err := service.PlanCleanup(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		e.Plan = plan
	case "cleanup-apply":
		if opts.dryRun || path == "" {
			e.fail("USAGE", "network cleanup-apply requires --file CLEANUP-PLAN.json and does not accept --dry-run")
			return
		}
		var plan networking.CleanupPlan
		if err := readNetworkJSON(path, &plan); err != nil {
			failError(e, err)
			return
		}
		status, err := service.ApplyCleanup(ctx, plan)
		if err != nil {
			if errors.Is(err, networking.ErrConflict) {
				e.fail("STATE_CONFLICT", "network ownership or settings changed after this cleanup plan; inspect and create a new plan")
			} else if strings.Contains(err.Error(), "failed after") {
				e.Status = "partial"
				e.Errors = append(e.Errors, commandError{Code: "TUNNEL_FAILED", Message: err.Error(), Actions: []string{"run `rundevrun network setup-check --json`, inspect owned resources, then create a new cleanup plan"}})
			} else {
				failError(e, err)
			}
			return
		}
		e.Results = append(e.Results, status)
	case "setup-apply":
		if opts.dryRun || path == "" {
			e.fail("USAGE", "network setup-apply requires --file SETUP-PLAN.json and does not accept --dry-run")
			return
		}
		var plan networking.SetupPlan
		if err := readNetworkJSON(path, &plan); err != nil {
			failError(e, err)
			return
		}
		if !plan.TermsAccepted {
			e.fail("USAGE", "review the certificate authority terms at the plan's terms_url, then set terms_accepted to true")
			return
		}
		snapshot, err := service.Inspect(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		_, configPath, err := machineRouting()
		if err != nil {
			failError(e, err)
			return
		}
		activation, err := networkactivation.Prepare(ctx, store, executablePath(), configPath, snapshot.Config)
		if err != nil {
			failError(e, err)
			return
		}
		defer activation.Abort()
		result, err := service.ApplySetup(ctx, plan)
		if err != nil {
			if errors.Is(err, networking.ErrConflict) {
				e.fail("STATE_CONFLICT", "network settings changed after this setup plan; inspect and create a new plan")
			} else if strings.Contains(err.Error(), "failed after") {
				e.Status = "partial"
				e.Errors = append(e.Errors, commandError{Code: "TUNNEL_FAILED", Message: err.Error(), Actions: []string{"run `rundevrun network setup-check --json`, inspect the named resource, then use setup-recover before retrying"}})
			} else {
				failError(e, err)
			}
			return
		}
		e.Results = append(e.Results, result)
		if err := activation.Commit(ctx); err != nil {
			e.Status = "partial"
			e.Errors = append(e.Errors, commandError{Code: "ROUTE_RECONCILE_FAILED", Message: "tailnet setup completed, but route commit failed: " + err.Error(), Actions: []string{"inspect `rundevrun network check --json` and reconcile routes after correcting the error"}})
		} else if err := activation.Finish(ctx); err != nil {
			e.Status = "partial"
			e.Errors = append(e.Errors, commandError{Code: "ROUTE_RECONCILE_FAILED", Message: "tailnet setup completed, but local route reconciliation failed: " + err.Error(), Actions: []string{"inspect `rundevrun network check --json` and reconcile routes after correcting the error"}})
		}
	case "credential-store", "credential-replace":
		if path != "" || opts.dryRun || len(args) != 3 {
			e.fail("USAGE", "usage: rundevrun network credential-store REF, with the token supplied on stdin")
			return
		}
		ref := args[2]
		if strings.TrimSpace(ref) != ref || ref == "" {
			e.fail("USAGE", "credential reference must be a nonempty file name")
			return
		}
		token, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024+1))
		if err != nil {
			failError(e, err)
			return
		}
		if len(token) == 0 || len(token) > 64*1024 {
			e.fail("USAGE", "credential input must contain 1 to 65536 bytes")
			return
		}
		defer clear(token)
		if command == "credential-replace" {
			err = service.ReplaceCredential(ref, token)
		} else {
			err = service.StoreCredential(ref, token)
		}
		if err != nil {
			failError(e, err)
			return
		}
		e.Results = append(e.Results, map[string]string{"credential_ref": ref, "stored": "true"})
	case "setup-recover":
		if len(args) != 2 || path != "" || opts.dryRun {
			e.fail("USAGE", "network setup-recover does not accept arguments, --file, or --dry-run")
			return
		}
		status, err := service.RecoverSetup(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		e.Results = append(e.Results, status)
	case "resolve-finalization":
		if len(args) != 2 || path != "" || opts.dryRun {
			e.fail("USAGE", "network resolve-finalization does not accept arguments, --file, or --dry-run")
			return
		}
		status, err := service.ResolvePendingFinalization(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		e.Results = append(e.Results, status)
	case "adopt-dns", "adopt-existing-dns", "resolve-challenge":
		if len(args) != 3 || path != "" || opts.dryRun || strings.TrimSpace(args[2]) != args[2] || args[2] == "" {
			e.fail("USAGE", "network adopt-dns and resolve-challenge require the inspected resource ID")
			return
		}
		var status networking.SetupStatus
		if command == "adopt-dns" {
			status, err = service.AdoptPendingDNS(ctx, args[2])
		} else if command == "adopt-existing-dns" {
			status, err = service.AdoptExistingDNS(ctx, args[2])
		} else {
			status, err = service.ResolvePendingChallenge(ctx, args[2])
		}
		if err != nil {
			failError(e, err)
			return
		}
		e.Results = append(e.Results, status)
	case "adopt-serve":
		if len(args) != 2 || path != "" || opts.dryRun {
			e.fail("USAGE", "network adopt-serve does not accept arguments, --file, or --dry-run")
			return
		}
		status, err := service.AdoptPendingServe(ctx)
		if err != nil {
			failError(e, err)
			return
		}
		e.Results = append(e.Results, status)
	default:
		e.fail("USAGE", "unknown network command: "+command)
	}
}

type networkPlanFile struct {
	Settings      networking.Plan           `json:"settings"`
	RouteRevision uint64                    `json:"route_revision"`
	RouteChanges  []runner.LocalRouteChange `json:"route_changes"`
}

func rollbackNetworkSettings(ctx context.Context, service networking.Service, previous networking.Config, expectedRevision string) error {
	_, err := service.Apply(ctx, networking.Plan{Baseline: expectedRevision, Desired: previous})
	return err
}

func readNetworkJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("read network JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("read network JSON: trailing data")
	}
	return nil
}
