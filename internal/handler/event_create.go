package handler

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/rhpds/babylon-runner/internal/runner"
	"github.com/rhpds/babylon-runner/internal/types"
)

// govVarWithDefault returns the string value of key from governor job_vars,
// or defaultVal if missing, nil, or empty.
func govVarWithDefault(govJV map[string]interface{}, key, defaultVal string) string {
	if govJV == nil {
		return defaultVal
	}
	if v, ok := govJV[key].(string); ok && v != "" {
		return v
	}
	return defaultVal
}

// buildInitialJobVars constructs the initial job_vars patch for a newly
// created subject, resolving cloud_provider, platform, uuid, and guid.
func buildInitialJobVars(rc *runner.RunContext) map[string]interface{} {
	govJV := rc.GovernorJobVars()

	cloudProvider := govVarWithDefault(govJV, "cloud_provider", "none")
	platform := govVarWithDefault(govJV, "platform", "RHPDS")

	// uuid: use existing subject uuid if set, otherwise generate.
	subjectUUID := uuid.New().String()
	if sjv := rc.JobVars(); sjv != nil {
		if u, ok := sjv["uuid"].(string); ok && u != "" {
			subjectUUID = u
		}
	}

	vars := map[string]interface{}{
		"cloud_provider": cloudProvider,
		"platform":       platform,
		"uuid":           subjectUUID,
	}

	// guid: only set if not already present (defaults to uuid).
	sjv := rc.JobVars()
	if sjv == nil {
		vars["guid"] = subjectUUID
	} else if _, ok := sjv["guid"].(string); !ok {
		vars["guid"] = subjectUUID
	}

	return vars
}

// handleEventCreate handles the "create" subject event. It initializes a
// newly created subject by setting cloud_provider, platform, uuid in
// job_vars (matching Ansible's handle-event-create.yaml), then scheduling
// the provision action.
func handleEventCreate(ctx context.Context, rc *runner.RunContext) error {
	slog.Info("handling create event", "subject", rc.SubjectName())

	// Initialize subject if not already done.
	if rc.CurrentState() == "" {
		jobVarsPatch := buildInitialJobVars(rc)

		if err := rc.SubjectUpdate(ctx, types.SubjectPatch{
			Patch: types.PatchBody{
				Metadata: &types.PatchMetadata{
					Labels: map[string]string{
						"state": "provision-pending",
					},
				},
				Spec: &types.PatchSpec{
					Vars: map[string]interface{}{
						"current_state": "provision-pending",
						"job_vars":      jobVarsPatch,
					},
				},
				SkipUpdateProcessing: true,
			},
		}); err != nil {
			return err
		}
	}

	slog.Info("scheduling provision action", "subject", rc.SubjectName())
	return rc.ScheduleAction(ctx, types.ScheduleActionRequest{
		Action: "provision",
	})
}
