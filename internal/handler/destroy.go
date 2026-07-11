package handler

import (
	"context"
	"log/slog"

	"github.com/rhpds/babylon-runner/internal/runner"
	"github.com/rhpds/babylon-runner/internal/types"
)

// handleDestroy routes a destroy action based on the current state.
func handleDestroy(ctx context.Context, rc *runner.RunContext) error {
	slog.Info("handling destroy", "subject", rc.SubjectName(), "state", rc.CurrentState())
	currentState := rc.CurrentState()
	actions := rc.StatusActions()
	destroy := types.GetNestedMap(actions, "destroy")

	// Set startTimestamp if not already set and we're in destroy-pending.
	if (destroy == nil || destroy["startTimestamp"] == nil) && currentState == "destroy-pending" {
		ts := types.NowUTC()
		if err := rc.SubjectUpdate(ctx, types.SubjectPatch{
			Patch: types.PatchBody{
				Status: map[string]interface{}{
					"actions": map[string]interface{}{
						"destroy": map[string]interface{}{
							"startTimestamp": ts,
						},
					},
				},
				SkipUpdateProcessing: true,
			},
		}); err != nil {
			return err
		}
	}

	// Sandbox API destroy catch-all: cleanup and delete if in error state.
	if rc.SandboxAPIInUse() && sandboxDestroyCatchAll(rc) {
		errorStates := map[string]bool{
			"destroy-error":    true,
			"destroy-failed":   true,
			"destroy-canceled": true,
		}
		if errorStates[currentState] || rc.DeployerDisabled("destroy") {
			slog.Info("handleDestroy: destroy catch-all triggered", "subject", rc.SubjectName(), "state", currentState)
			return handleDestroyCatchAll(ctx, rc)
		}
	}

	// Run destroy if not yet in "destroying" state.
	if currentState != "destroying" && !rc.DeployerDisabled("destroy") {
		return runDestroy(ctx, rc)
	}

	// Check deployer job if already destroying.
	if currentState == "destroying" && !rc.DeployerDisabled("destroy") {
		return checkDeployerJob(ctx, rc, "destroy")
	}

	return nil
}

// handleDestroyCatchAll performs sandbox cleanup and subject deletion for the
// destroy catch-all path (error states or deployer disabled).
func handleDestroyCatchAll(ctx context.Context, rc *runner.RunContext) error {
	if err := sandboxCleanup(ctx, rc); err != nil {
		slog.Error("handleDestroyCatchAll: sandbox cleanup error", "subject", rc.SubjectName(), "error", err)
	}
	rc.DeleteSubject(true)
	rc.FinishAction("successful")
	return nil
}

// runDestroy initiates the destroy workflow.
func runDestroy(ctx context.Context, rc *runner.RunContext) error {
	// Sandbox API integration: get placement for destroy vars.
	var dynamicJobVars map[string]interface{}
	if rc.SandboxAPIInUse() {
		result, err := sandboxGet(ctx, rc, "destroy")
		if err != nil {
			slog.Error("runDestroy: sandbox get error", "subject", rc.SubjectName(), "error", err)
		} else if result != nil {
			if result.Status == "error" {
				slog.Error("runDestroy: sandbox placement in error state", "subject", rc.SubjectName())
			}
			dynamicJobVars = result.DynamicVars
		}
	}

	// Cancel running provision Tower job if exists.
	cancelTowerJob(ctx, rc, "provision")

	// Launch Tower job for destroy.
	if err := launchTowerJob(ctx, rc, "destroy", "destroying", nil, dynamicJobVars); err != nil {
		slog.Error("runDestroy: tower launch failed", "subject", rc.SubjectName(), "error", err)
		return err
	}

	rc.ContinueAction(rc.TowerPollIntervals[0])
	return nil
}

// handleDestroyComplete finalizes a successful destroy.
func handleDestroyComplete(ctx context.Context, rc *runner.RunContext) error {
	slog.Info("destroy complete", "subject", rc.SubjectName())
	// Sandbox API cleanup: release placement.
	if rc.SandboxAPIInUse() {
		if err := sandboxCleanup(ctx, rc); err != nil {
			slog.Error("handleDestroyComplete: sandbox cleanup error", "subject", rc.SubjectName(), "error", err)
		}
	}

	ts := types.NowUTC()

	if err := rc.SubjectUpdate(ctx, types.SubjectPatch{
		Patch: types.PatchBody{
			Metadata: &types.PatchMetadata{
				Labels: map[string]string{
					"state": "destroy-complete",
				},
			},
			Spec: &types.PatchSpec{
				Vars: map[string]interface{}{
					"current_state": "destroy-complete",
				},
			},
			Status: map[string]interface{}{
				"actions": map[string]interface{}{
					"destroy": map[string]interface{}{
						"completeTimestamp": ts,
						"state":            "successful",
					},
				},
				"towerJobs": map[string]interface{}{
					"destroy": map[string]interface{}{
						"completeTimestamp": ts,
						"jobStatus":        "successful",
					},
				},
			},
			SkipUpdateProcessing: true,
		},
	}); err != nil {
		return err
	}

	rc.DeleteSubject(true)
	rc.FinishAction("successful")
	return nil
}
