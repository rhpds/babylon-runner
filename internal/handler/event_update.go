package handler

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/rhpds/babylon-runner/internal/runner"
	"github.com/rhpds/babylon-runner/internal/types"
)

// determineUpdateAction decides which action (update/start/stop) to take
// based on job_vars changes and state transitions. Returns "" if no action
// is needed.
func determineUpdateAction(currentJobVars, previousJobVars map[string]interface{}, currentState, desiredState string) string {
	if !reflect.DeepEqual(currentJobVars, previousJobVars) {
		return "update"
	}
	if desiredState == "started" && currentState == "stopped" {
		return "start"
	}
	if desiredState == "stopped" && currentState == "started" {
		return "stop"
	}
	return ""
}

// shouldScheduleStatus returns true if a status check should be scheduled.
// Two paths (matching Ansible):
//  1. Legacy: check_status_state set to "pending" directly (no timestamp).
//  2. Timestamp: new request_timestamp differs from last, and state is empty or "successful".
func shouldScheduleStatus(checkStatusState, currentTimestamp, previousTimestamp string) bool {
	if checkStatusState == "pending" && currentTimestamp == "" {
		return true
	}
	if currentTimestamp != "" && currentTimestamp != previousTimestamp &&
		(checkStatusState == "" || checkStatusState == "successful") {
		return true
	}
	return false
}

// scheduleUpdateAction patches the subject state and schedules the given action.
func scheduleUpdateAction(ctx context.Context, rc *runner.RunContext, action string) error {
	pendingState := action + "-pending"
	if err := rc.SubjectUpdate(ctx, types.SubjectPatch{
		Patch: types.PatchBody{
			Metadata: &types.PatchMetadata{
				Labels: map[string]string{
					"state": pendingState,
				},
			},
			Spec: &types.PatchSpec{
				Vars: map[string]interface{}{
					"current_state": pendingState,
				},
			},
			SkipUpdateProcessing: true,
		},
	}); err != nil {
		return err
	}

	slog.Info("scheduling action from update event", "action", action, "subject", rc.SubjectName())
	return rc.ScheduleAction(ctx, types.ScheduleActionRequest{
		Action: action,
		Cancel: []string{"start", "stop"},
	})
}

// scheduleStatusCheck patches check_status_state and schedules the status action.
func scheduleStatusCheck(ctx context.Context, rc *runner.RunContext) error {
	if err := rc.SubjectUpdate(ctx, types.SubjectPatch{
		Patch: types.PatchBody{
			Spec: &types.PatchSpec{
				Vars: map[string]interface{}{
					"check_status_state": "pending",
				},
			},
			SkipUpdateProcessing: true,
		},
	}); err != nil {
		return err
	}

	return rc.ScheduleAction(ctx, types.ScheduleActionRequest{
		Action: "status",
	})
}

// tryScheduleAction schedules the action if it is supported by the governor.
func tryScheduleAction(ctx context.Context, rc *runner.RunContext, action string) error {
	if action == "" {
		return nil
	}
	govActions := rc.GovernorActions()
	if govActions == nil {
		return nil
	}
	if _, ok := govActions[action]; !ok {
		return nil
	}
	return scheduleUpdateAction(ctx, rc, action)
}

// previousCheckStatusTimestamp extracts the previous check_status_request_timestamp.
func previousCheckStatusTimestamp(previousState map[string]interface{}) string {
	if previousState == nil {
		return ""
	}
	ts, _ := previousState["check_status_request_timestamp"].(string)
	return ts
}

// handleEventUpdate handles the "update" subject event. It determines
// whether an action (update/start/stop) is needed based on state and
// job_vars changes, and checks for status requests.
func handleEventUpdate(ctx context.Context, rc *runner.RunContext) error {
	slog.Info("handling update event", "subject", rc.SubjectName(), "currentState", rc.CurrentState(), "desiredState", rc.DesiredState())

	currentJobVars := rc.JobVars()
	if currentJobVars == nil {
		currentJobVars = make(map[string]interface{})
	}

	previousJobVars := types.GetNestedMap(rc.Payload.Subject.Status.PreviousState, "job_vars")
	if previousJobVars == nil {
		previousJobVars = currentJobVars
	}

	action := determineUpdateAction(currentJobVars, previousJobVars, rc.CurrentState(), rc.DesiredState())
	if err := tryScheduleAction(ctx, rc, action); err != nil {
		return err
	}

	govActions := rc.GovernorActions()
	if govActions == nil {
		return nil
	}
	if _, hasStatus := govActions["status"]; !hasStatus {
		return nil
	}

	if shouldScheduleStatus(
		rc.CheckStatusState(),
		rc.Payload.Subject.Spec.Vars.GetString("check_status_request_timestamp"),
		previousCheckStatusTimestamp(rc.Payload.Subject.Status.PreviousState),
	) {
		return scheduleStatusCheck(ctx, rc)
	}

	return nil
}
