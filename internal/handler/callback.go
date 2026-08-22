package handler

import (
	"context"
	"log/slog"

	"github.com/rhpds/babylon-runner/internal/runner"
)

// actionNames lists the actions that support callbacks. These mirror the
// Ansible governor's handle-action-{action}-{callback}.yaml files.
var actionNames = []string{"provision", "destroy", "start", "stop", "status", "update"}

// callbackStatuses lists the terminal callback statuses. Each action accepts
// every one of these, matching the Ansible 6 actions x 4 callbacks = 24 handlers.
var callbackStatuses = []string{"canceled", "complete", "error", "failed"}

// callbackPayloadData extracts the deployer callback payload fields
// (data, message_body, messages) from the run handler vars. This matches the
// Ansible variables:
//
//	action_provision_data        = anarchy_action_callback_data.data
//	action_provision_message_body = anarchy_action_callback_data.message_body
//	action_provision_messages    = anarchy_action_callback_data.messages
//
// The Anarchy API stores the raw POST body in handler.vars under the key
// "anarchy_action_callback_data".
func callbackPayloadData(rc *runner.RunContext) (data, messageBody, messages interface{}) {
	if rc.Payload.Handler.Vars == nil {
		return nil, nil, nil
	}
	raw, ok := rc.Payload.Handler.Vars["anarchy_action_callback_data"]
	if !ok {
		return nil, nil, nil
	}
	cd, ok := raw.(map[string]interface{})
	if !ok {
		return nil, nil, nil
	}
	return cd["data"], cd["message_body"], cd["messages"]
}

// handleCallback dispatches a deployer callback run to the appropriate
// completion or failure handler for the given action and status.
//
// This is the Go equivalent of the Ansible handle-action-{action}-{callback}.yaml
// include, which the operator triggers for every callback (not only "complete").
// For "complete" it forwards the callback payload (provision_data, messages)
// into the completion handler; for the failure statuses it reuses the same
// failure handlers used by the Tower polling path (checkDeployerJob), which
// already mirror the Ansible error/failed/canceled handlers.
func handleCallback(ctx context.Context, rc *runner.RunContext, action, status string) error {
	slog.Info("handling action callback",
		"action", action, "callback", status, "subject", rc.SubjectName())

	if status == "complete" {
		data, messageBody, messages := callbackPayloadData(rc)
		switch action {
		case "provision":
			return handleProvisionComplete(ctx, rc, data, messageBody, messages)
		case "destroy":
			return handleDestroyComplete(ctx, rc)
		case "start":
			return handleStartComplete(ctx, rc)
		case "stop":
			return handleStopComplete(ctx, rc)
		case "status":
			return handleStatusComplete(ctx, rc, data, messages)
		case "update":
			return handleUpdateComplete(ctx, rc)
		}
		return nil
	}

	return handleDeployerJobFailure(ctx, rc, action, status)
}
