package handler

import (
	"context"

	"github.com/rhpds/babylon-runner/internal/runner"
)

// Register returns the handler map for all supported run types.
func Register() map[string]runner.HandlerFunc {
	handlers := map[string]runner.HandlerFunc{
		// Event handlers
		"event:create": handleEventCreate,
		"event:update": handleEventUpdate,
		"event:delete": handleEventDelete,

		// Action handlers
		"action:provision": handleProvision,
		"action:destroy":   handleDestroy,
		"action:start":     handleStart,
		"action:stop":      handleStop,
		"action:status":    handleStatus,
		"action:update":    handleUpdate,
	}

	// Register a handler for every action callback (canceled/complete/error/failed).
	// This matches the 24 handle-action-{action}-{callback}.yaml files in the
	// Ansible governor and ensures error/failed/canceled callbacks are dispatched
	// instead of being dropped.
	for _, action := range actionNames {
		for _, status := range callbackStatuses {
			action, status := action, status
			handlers["action:"+action+":"+status] = func(ctx context.Context, rc *runner.RunContext) error {
				return handleCallback(ctx, rc, action, status)
			}
		}
	}

	return handlers
}
