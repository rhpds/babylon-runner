package handler

import (
	"context"
	"log/slog"

	"github.com/rhpds/babylon-runner/internal/runner"
)

// handleUpdate routes an update action based on the current state.
func handleUpdate(ctx context.Context, rc *runner.RunContext) error {
	slog.Info("handling update", "subject", rc.SubjectName(), "state", rc.CurrentState())
	currentState := rc.CurrentState()

	if currentState != "updating" {
		return runUpdate(ctx, rc)
	}

	return checkDeployerJob(ctx, rc, "update")
}

// runUpdate initiates the update workflow.
func runUpdate(ctx context.Context, rc *runner.RunContext) error {
	// Get sandbox vars for Tower job.
	var dynamicJobVars map[string]interface{}
	if rc.SandboxAPIInUse() {
		result, err := sandboxGet(ctx, rc, "update")
		if err != nil {
			slog.Error("runUpdate: sandbox get error", "subject", rc.SubjectName(), "error", err)
		} else if result != nil {
			dynamicJobVars = result.DynamicVars
		}
	}

	// Launch Tower job for update.
	if err := launchTowerJob(ctx, rc, "update", "updating", nil, dynamicJobVars); err != nil {
		slog.Error("runUpdate: tower launch failed", "subject", rc.SubjectName(), "error", err)
		return err
	}
	rc.ContinueAction(rc.TowerPollIntervals[0])
	return nil
}
