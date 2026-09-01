package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rhpds/babylon-runner/internal/clients"
	"github.com/rhpds/babylon-runner/internal/httputil"
	"github.com/rhpds/babylon-runner/internal/runner"
	"github.com/rhpds/babylon-runner/internal/template"
	"github.com/rhpds/babylon-runner/internal/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxResult holds the result of a sandbox API get or book operation.
type SandboxResult struct {
	Status      string                 // "success", "queued", "error", "not-found"
	Placement   map[string]interface{} // raw placement data
	DynamicVars map[string]interface{} // extracted job vars (with creds) for Tower extra_vars
	SubjectVars map[string]interface{} // extracted vars (no creds) for subject/provision_data
	Labels      map[string]string      // extracted labels
}

// sandboxAPIVars returns the sandbox_api varSecret data from
// governor.spec.vars.sandbox_api. This is injected by the Anarchy operator
// from a K8s Secret configured in the governor's varSecrets.
func sandboxAPIVars(rc *runner.RunContext) map[string]interface{} {
	return rc.Payload.Governor.Spec.Vars.SandboxAPI
}

// getSandboxClient creates a SandboxAPIClient with integrated login.
// URL priority: rc.SandboxBaseURL (test) > governor varSecret > rc.DefaultSandboxAPIURL (config).
func getSandboxClient(rc *runner.RunContext) (*clients.SandboxAPIClient, error) {
	loginToken := sandboxLoginToken(rc)
	if loginToken == "" {
		return nil, fmt.Errorf("no sandbox_api_login_token in governor sandbox_api secret")
	}

	baseURL := rc.DefaultSandboxAPIURL
	if rc.SandboxBaseURL != "" {
		baseURL = rc.SandboxBaseURL
	} else {
		sbVars := sandboxAPIVars(rc)
		if sbVars != nil {
			if u, ok := sbVars["sandbox_api_url"].(string); ok && u != "" {
				baseURL = u
			}
		}
	}
	return clients.NewSandboxAPIClient(baseURL, loginToken, rc.SandboxClientOpts...), nil
}

// sandboxLoginToken returns the sandbox API login token from
// governor.spec.vars.sandbox_api.sandbox_api_login_token (varSecret).
// Matches the Ansible default: {{ sandbox_api.sandbox_api_login_token }}.
func sandboxLoginToken(rc *runner.RunContext) string {
	sbVars := sandboxAPIVars(rc)
	if sbVars == nil {
		return ""
	}
	token, _ := sbVars["sandbox_api_login_token"].(string)
	return token
}

// sandboxGet performs the sandbox get workflow:
//  1. Create sandbox API client (with integrated login)
//  2. GET placement by UUID
//  3. If error status -> return error result
//  4. If found with resources -> extract vars, update subject
//  5. If action is "provision" and not found -> book new placement
func sandboxGet(ctx context.Context, rc *runner.RunContext, action string) (*SandboxResult, error) {
	uuid := rc.UUID()
	if uuid == "" {
		return nil, fmt.Errorf("no uuid in job_vars")
	}

	client, err := getSandboxClient(rc)
	if err != nil {
		return nil, err
	}

	placement, statusCode, err := client.GetPlacement(ctx, uuid)
	if err != nil {
		return nil, fmt.Errorf("get placement: %w", err)
	}

	// Not found -- book if provision action.
	if statusCode == http.StatusNotFound {
		if action == "provision" && sandboxLoginToken(rc) != "" {
			return sandboxBook(ctx, rc, client)
		}
		return &SandboxResult{Status: "not-found"}, nil
	}

	// Check placement status.
	placementStatus, _ := placement["status"].(string)
	if placementStatus == "error" {
		return &SandboxResult{Status: "error", Placement: placement}, nil
	}
	if placementStatus == "queued" {
		return &SandboxResult{Status: "queued", Placement: placement}, nil
	}

	// Extract vars (without creds for subject) and labels.
	subjectVars := extractSandboxVars(placement, false)
	labels := extractSandboxLabels(placement)
	// Extract vars with creds for Tower extra_vars.
	dynamicVars := extractSandboxVars(placement, true)

	if err := updateSubjectSandboxVars(ctx, rc, subjectVars, labels); err != nil {
		return nil, fmt.Errorf("update subject with sandbox vars: %w", err)
	}

	return &SandboxResult{
		Status:      "success",
		Placement:   placement,
		DynamicVars: dynamicVars,
		SubjectVars: subjectVars,
		Labels:      labels,
	}, nil
}

// sandboxBook books a new placement via the sandbox API.
// The client is passed from the caller (sandboxGet) so the same
// token cache is reused.
func sandboxBook(ctx context.Context, rc *runner.RunContext, client *clients.SandboxAPIClient) (*SandboxResult, error) {
	uuid := rc.UUID()
	guid := rc.GUID()
	meta := rc.Meta()

	// Build request body.
	reqBody := map[string]interface{}{
		"service_uuid": uuid,
	}

	// Add reservation from __meta__.sandbox_api.reservation.
	if meta != nil && meta.SandboxAPI != nil {
		if reservation, ok := meta.SandboxAPI["reservation"].(string); ok && reservation != "" {
			reqBody["reservation"] = reservation
		}
	}

	// Add annotations.
	annotations := map[string]interface{}{
		"guid": guid,
	}
	govJobVars := rc.GovernorJobVars()
	if govJobVars != nil {
		if envType, ok := govJobVars["env_type"].(string); ok {
			annotations["env_type"] = envType
		}
	}
	subjectJobVars := rc.JobVars()
	if subjectJobVars != nil {
		// owner = requester_username or student_name or "babylon"
		owner := types.FirstString(
			rc.Payload.Subject.Metadata.Annotations["poolboy.gpte.redhat.com/resource-requester-user"],
			types.StringFromMap(subjectJobVars, "requester_username"),
			types.StringFromMap(subjectJobVars, "student_name"),
			"babylon",
		)
		annotations["owner"] = owner

		// owner_email = requester_email or email or "unknown"
		email := types.FirstString(
			rc.Payload.Subject.Metadata.Annotations["poolboy.gpte.redhat.com/resource-requester-email"],
			types.StringFromMap(subjectJobVars, "requester_email"),
			types.StringFromMap(subjectJobVars, "email"),
			"unknown",
		)
		annotations["owner_email"] = email
	}
	// comment includes the OCP console URL (best-effort), matching the Ansible
	// sandbox_api_book.yaml which reads the console-public ConfigMap.
	annotations["comment"] = "sandbox-api " + ocpConsoleURL(ctx, rc)
	reqBody["annotations"] = annotations

	// Add sandboxes/resources from __meta__ with var annotations injected.
	// Resolve Jinja2 expressions (e.g. {{ job_vars.namespace_suffix | default('dev') }})
	// before sending to the sandbox API.
	if meta != nil && meta.Sandboxes != nil {
		vars := template.J2VarContext(rc.SubjectAllVars(), rc.GovernorAllVars())
		resolved := template.ResolveJ2(meta.Sandboxes, vars).([]interface{})

		// Validate the request before sending it to the sandbox API,
		// matching the Ansible sandbox_api_book.yaml
		// validate_sandboxes_request task: on an invalid request the action
		// finishes as failed without retrying (fail + anarchy_finish_action
		// failed + end_play).
		if msg := validateSandboxesRequest(resolved); msg != "OK" {
			slog.Error("sandboxBook: invalid sandboxes request", "subject", rc.SubjectName(), "error", msg)
			rc.FinishAction("failed")
			return &SandboxResult{Status: "invalid-request"}, nil
		}

		reqBody["resources"] = injectVarAnnotations(resolved)
	}

	result, statusCode, err := client.BookPlacement(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("book placement: %w", err)
	}

	switch statusCode {
	case http.StatusOK:
		dynamicVars := extractSandboxVars(result, true)
		labels := extractSandboxLabels(result)
		return &SandboxResult{
			Status:      "success",
			Placement:   result,
			DynamicVars: dynamicVars,
			Labels:      labels,
		}, nil
	case 202, 507:
		// Queued or no capacity.
		return &SandboxResult{Status: "queued", Placement: result}, nil
	default:
		return &SandboxResult{Status: "error", Placement: result},
			fmt.Errorf("book placement returned status %d", statusCode)
	}
}

// ocpConsoleURL returns the OCP console URL from the console-public ConfigMap
// in the openshift-config-managed namespace, or "" if unavailable. Best-effort:
// missing cluster access or ConfigMap is not fatal, matching the Ansible
// sandbox_api_book.yaml which defaults to ” when the read fails.
func ocpConsoleURL(ctx context.Context, rc *runner.RunContext) string {
	if rc.Clientset == nil {
		return ""
	}
	cm, err := rc.Clientset.CoreV1().ConfigMaps("openshift-config-managed").Get(ctx, "console-public", metav1.GetOptions{})
	if err != nil {
		slog.Debug("ocpConsoleURL: unable to read console-public ConfigMap", "error", err)
		return ""
	}
	if v, ok := cm.Data["consoleURL"]; ok {
		return v
	}
	return ""
}

// sandboxCleanup releases the sandbox placement.
// It asserts that both guid and uuid are defined and non-empty, matching the
// Ansible sandbox_cleanup.yml asserts (guid and uuid must be defined and != ”).
func sandboxCleanup(ctx context.Context, rc *runner.RunContext) error {
	uuid := rc.UUID()
	if uuid == "" {
		return fmt.Errorf("sandbox cleanup: uuid is not defined or empty")
	}
	if rc.GUID() == "" {
		return fmt.Errorf("sandbox cleanup: guid is not defined or empty")
	}

	token := sandboxLoginToken(rc)
	if token == "" {
		slog.Warn("sandboxCleanup: no login token, skipping")
		return nil
	}

	client, err := getSandboxClient(rc)
	if err != nil {
		return fmt.Errorf("sandbox cleanup: %w", err)
	}

	if err := client.ReleasePlacement(ctx, uuid); err != nil {
		return fmt.Errorf("release placement: %w", err)
	}

	slog.Info("sandboxCleanup: released placement", "uuid", uuid)
	return nil
}

// sandboxStart starts the sandbox placement and polls for completion.
// It records the request status in subject.status.sandboxAPIJobs.start,
// matching the Ansible sandbox_api_start.yaml status shape.
func sandboxStart(ctx context.Context, rc *runner.RunContext) error {
	uuid := rc.UUID()
	if uuid == "" {
		err := fmt.Errorf("no uuid for sandbox start")
		recordSandboxActionFailure(ctx, rc, "start", 0, err)
		return err
	}

	client, err := getSandboxClient(rc)
	if err != nil {
		return err
	}

	result, httpStatus, err := client.StartPlacement(ctx, uuid)
	if err != nil {
		err = fmt.Errorf("start placement: %w", err)
		recordSandboxActionFailure(ctx, rc, "start", httpStatus, err)
		return err
	}

	// Extract request_id and message for polling / status.
	requestID, _ := result["request_id"].(string)
	message, _ := result["message"].(string)

	// Update subject status with request info (message + httpStatus).
	if err := rc.SubjectUpdate(ctx, types.SubjectPatch{
		Patch: types.PatchBody{
			Metadata: &types.PatchMetadata{
				Labels: map[string]string{"state": "starting"},
			},
			Status: map[string]interface{}{
				"sandboxAPIJobs": map[string]interface{}{
					"start": map[string]interface{}{
						"requestID":      requestID,
						"message":        message,
						"startTimestamp": types.NowUTC(),
						"httpStatus":     httpStatus,
					},
				},
			},
			SkipUpdateProcessing: true,
		},
	}); err != nil {
		recordSandboxActionFailure(ctx, rc, "start", httpStatus, err)
		return err
	}

	// No request_id means the operation completed synchronously.
	if requestID == "" {
		slog.Warn("sandboxStart: no request_id in response, skipping poll", "subject", rc.SubjectName())
		return nil
	}

	return pollSandboxRequest(ctx, rc, client, requestID, "start")
}

// sandboxStop stops the sandbox placement and polls for completion.
// It records the request status in subject.status.sandboxAPIJobs.stop,
// matching the Ansible sandbox_api_stop.yaml status shape.
func sandboxStop(ctx context.Context, rc *runner.RunContext) error {
	uuid := rc.UUID()
	if uuid == "" {
		err := fmt.Errorf("no uuid for sandbox stop")
		recordSandboxActionFailure(ctx, rc, "stop", 0, err)
		return err
	}

	client, err := getSandboxClient(rc)
	if err != nil {
		return err
	}

	result, httpStatus, err := client.StopPlacement(ctx, uuid)
	if err != nil {
		err = fmt.Errorf("stop placement: %w", err)
		recordSandboxActionFailure(ctx, rc, "stop", httpStatus, err)
		return err
	}

	// Extract request_id and message for polling / status.
	requestID, _ := result["request_id"].(string)
	message, _ := result["message"].(string)

	// Update subject status with request info (message + httpStatus).
	if err := rc.SubjectUpdate(ctx, types.SubjectPatch{
		Patch: types.PatchBody{
			Metadata: &types.PatchMetadata{
				Labels: map[string]string{"state": "stopping"},
			},
			Status: map[string]interface{}{
				"sandboxAPIJobs": map[string]interface{}{
					"stop": map[string]interface{}{
						"requestID":      requestID,
						"message":        message,
						"startTimestamp": types.NowUTC(),
						"httpStatus":     httpStatus,
					},
				},
			},
			SkipUpdateProcessing: true,
		},
	}); err != nil {
		recordSandboxActionFailure(ctx, rc, "stop", httpStatus, err)
		return err
	}

	// No request_id means the operation completed synchronously.
	if requestID == "" {
		slog.Warn("sandboxStop: no request_id in response, skipping poll", "subject", rc.SubjectName())
		return nil
	}

	return pollSandboxRequest(ctx, rc, client, requestID, "stop")
}

// pollSandboxRequest polls a sandbox API async request until it completes.
// Uses httputil.PollWithContext for cancellation-aware polling. On terminal
// success it records jobStatus=successful and a completeTimestamp in
// subject.status.sandboxAPIJobs.{action}; on terminal failure it records the
// failing jobStatus, matching the Ansible sandbox_api_start/stop.yaml behavior.
func pollSandboxRequest(ctx context.Context, rc *runner.RunContext, client *clients.SandboxAPIClient, requestID, action string) error {
	return httputil.PollWithContext(ctx, 5*time.Second, 120, func() (bool, error) {
		status, err := client.GetRequestStatus(ctx, requestID)
		if err != nil {
			slog.Error("pollSandboxRequest: error checking request", "requestID", requestID, "error", err)
			// Transient error — keep polling.
			return false, nil
		}

		state, _ := status["status"].(string)
		ts := types.NowUTC()
		switch state {
		case "success", "complete":
			slog.Info("pollSandboxRequest: request completed successfully", "requestID", requestID, "action", action)
			return true, recordSandboxJobOutcome(ctx, rc, action, map[string]interface{}{
				"completeTimestamp": ts,
				"jobStatus":         "successful",
			})
		case "error", "failed":
			msg, _ := status["message"].(string)
			_ = recordSandboxJobOutcome(ctx, rc, action, map[string]interface{}{
				"completeTimestamp": ts,
				"jobStatus":         state,
				"message":           msg,
			})
			return true, fmt.Errorf("sandbox request %s failed: %s", requestID, msg)
		default:
			slog.Info("pollSandboxRequest: request still in progress", "requestID", requestID, "status", state, "action", action)
			return false, nil
		}
	})
}

// recordSandboxJobOutcome merges terminal outcome fields into
// subject.status.sandboxAPIJobs.{action} and returns nil on success, or the
// error so the caller can propagate a failure.
func recordSandboxJobOutcome(ctx context.Context, rc *runner.RunContext, action string, fields map[string]interface{}) error {
	return rc.SubjectUpdate(ctx, types.SubjectPatch{
		Patch: types.PatchBody{
			Status: map[string]interface{}{
				"sandboxAPIJobs": map[string]interface{}{
					action: fields,
				},
			},
			SkipUpdateProcessing: true,
		},
	})
}

// recordSandboxActionFailure is the Go equivalent of the rescue block in the
// Ansible sandbox_api_{start,stop}.yaml: when anything from the placement
// action onward fails, it best-effort records jobStatus=error (with
// httpStatus and message when available) in subject.status.sandboxAPIJobs.{action}
// so the failure leaves a persisted trace before the caller propagates the
// error. Recording failures are logged and swallowed — the original error is
// the one that matters.
func recordSandboxActionFailure(ctx context.Context, rc *runner.RunContext, action string, httpStatus int, err error) {
	fields := map[string]interface{}{
		"completeTimestamp": types.NowUTC(),
		"jobStatus":         "error",
		"message":           err.Error(),
	}
	if httpStatus > 0 {
		fields["httpStatus"] = httpStatus
	}
	if rerr := recordSandboxJobOutcome(ctx, rc, action, fields); rerr != nil {
		slog.Warn("recordSandboxActionFailure: unable to record failure status",
			"action", action, "subject", rc.SubjectName(), "error", rerr)
	}
}

// updateSubjectSandboxVars patches the subject with sandbox-extracted labels
// and merges non-secret vars into job_vars. It is a no-op when both maps are empty.
func updateSubjectSandboxVars(ctx context.Context, rc *runner.RunContext, subjectVars map[string]interface{}, labels map[string]string) error {
	if len(subjectVars) == 0 && len(labels) == 0 {
		return nil
	}
	patch := types.PatchBody{SkipUpdateProcessing: true}
	if len(labels) > 0 {
		patch.Metadata = &types.PatchMetadata{Labels: labels}
	}
	if len(subjectVars) > 0 {
		jv := rc.JobVars()
		if jv == nil {
			jv = make(map[string]interface{})
		}
		types.DeepMergeMap(jv, subjectVars)
		patch.Spec = &types.PatchSpec{
			Vars: map[string]interface{}{
				"job_vars": jv,
			},
		}
	}
	return rc.SubjectUpdate(ctx, types.SubjectPatch{Patch: patch})
}

// copyWithInjectedAnnotations returns a shallow copy of a sandbox resource
// map with "var" and "namespace_suffix" top-level keys injected into its
// annotations sub-map. Annotations are also shallow-copied to avoid mutation.
func copyWithInjectedAnnotations(sb map[string]interface{}) map[string]interface{} {
	cp := make(map[string]interface{}, len(sb))
	for k, v := range sb {
		cp[k] = v
	}

	ann, _ := cp["annotations"].(map[string]interface{})
	if ann == nil {
		ann = make(map[string]interface{})
	} else {
		annCopy := make(map[string]interface{}, len(ann))
		for k, v := range ann {
			annCopy[k] = v
		}
		ann = annCopy
	}

	if v, ok := cp["var"].(string); ok && v != "" {
		ann["var"] = v
	}
	if v, ok := cp["namespace_suffix"].(string); ok && v != "" {
		ann["namespace_suffix"] = v
	}
	if len(ann) > 0 {
		cp["annotations"] = ann
	}

	// Normalize boolean values in cloud_selector and cloud_preference to
	// "yes"/"no" strings (YAML parses unquoted true/false as booleans, but the
	// sandbox API requires string values). Matches the Python inject_var_annotations.
	normalizeBoolToYesNo(cp, "cloud_selector")
	normalizeBoolToYesNo(cp, "cloud_preference")

	return cp
}

// normalizeBoolToYesNo rewrites boolean values in the named sub-map of sb to
// the strings "yes"/"no", leaving non-boolean values untouched.
func normalizeBoolToYesNo(sb map[string]interface{}, key string) {
	sub, _ := sb[key].(map[string]interface{})
	if sub == nil {
		return
	}
	for k, v := range sub {
		if b, ok := v.(bool); ok {
			if b {
				sub[k] = "yes"
			} else {
				sub[k] = "no"
			}
		}
	}
}

// validateSandboxesRequest validates the resolved __meta__.sandboxes
// resources before they are sent to the sandbox API, matching the Python
// validate_sandboxes_request filter (babylon.py). It returns "OK" or an
// error message:
//   - at least one sandbox is required
//   - each sandbox must have a kind
//   - a sandbox with a var must not duplicate another sandbox's var
//   - a second sandbox of the same kind without a var is rejected
//   - annotations must be a dict of strings
//   - cloud_selector / cloud_preference must be dicts of strings or booleans
func validateSandboxesRequest(sandboxes []interface{}) string {
	if len(sandboxes) == 0 {
		return "ERROR: At least one sandbox is required in the sandboxes request"
	}

	mainFound := map[string]bool{}
	targetVars := map[string]bool{}

	for _, s := range sandboxes {
		if msg := validateSandbox(s, mainFound, targetVars); msg != "" {
			return msg
		}
	}
	return "OK"
}

// validateSandbox validates a single sandbox request entry, updating the
// main-found / target-var trackers. It returns an error message or "".
func validateSandbox(s interface{}, mainFound, targetVars map[string]bool) string {
	req, ok := s.(map[string]interface{})
	if !ok {
		return "ERROR: Sandbox request must be a dict"
	}
	if _, ok := req["kind"]; !ok {
		return "ERROR: Sandbox kind is required in the sandboxes request"
	}
	kind := fmt.Sprint(req["kind"])

	if msg := validateSandboxVar(req, kind, mainFound, targetVars); msg != "" {
		return msg
	}
	if msg := validateSandboxAnnotations(req, kind); msg != "" {
		return msg
	}
	for _, field := range []string{"cloud_selector", "cloud_preference"} {
		if msg := validateSandboxCloudField(req, field, kind); msg != "" {
			return msg
		}
	}
	return ""
}

// validateSandboxVar checks the var handling of one sandbox against the
// main-found / target-var trackers: a sandbox var must not duplicate another
// sandbox's var, and a second sandbox of the same kind without a var is
// rejected. It returns an error message or "".
func validateSandboxVar(req map[string]interface{}, kind string, mainFound, targetVars map[string]bool) string {
	// Python truthiness of req.get('var', False): missing, nil, false
	// and the empty string are falsy; anything else is truthy.
	if v, ok := req["var"]; ok && isTruthy(v) {
		varName := fmt.Sprint(v)
		if targetVars[varName] {
			return "ERROR: Variable '" + varName + "' is duplicated"
		}
		targetVars[varName] = true
		return ""
	}
	// A missing key reads as false (zero value), matching the Python
	// main_found.get(kind, False) default.
	if mainFound[kind] {
		return "ERROR: missing 'var' key for second sandbox of kind " + kind
	}
	mainFound[kind] = true
	return ""
}

// validateSandboxAnnotations checks that annotations, when present, are a
// dict of strings. It returns an error message or "".
func validateSandboxAnnotations(req map[string]interface{}, kind string) string {
	ann, ok := req["annotations"]
	if !ok {
		return ""
	}
	annMap, ok := ann.(map[string]interface{})
	if !ok {
		return "ERROR: Annotations should be a dict for sandbox of kind " + kind
	}
	for _, v := range annMap {
		if _, ok := v.(string); !ok {
			return "ERROR: Annotations values should be strings for sandbox of kind " + kind
		}
	}
	return ""
}

// validateSandboxCloudField checks that a cloud_selector / cloud_preference
// field, when present, is a dict of strings or booleans (booleans are
// normalized to "yes"/"no" by injectVarAnnotations). It returns an error
// message or "".
func validateSandboxCloudField(req map[string]interface{}, field, kind string) string {
	raw, ok := req[field]
	if !ok {
		return ""
	}
	fieldMap, ok := raw.(map[string]interface{})
	if !ok {
		return "ERROR: " + field + " should be a dict for sandbox of kind " + kind
	}
	for _, v := range fieldMap {
		switch v.(type) {
		case string, bool:
		default:
			return "ERROR: " + field + " values should be strings for sandbox of kind " + kind
		}
	}
	return ""
}

// isTruthy mirrors Python truthiness for the JSON/YAML value types that can
// appear in a sandbox "var" field: nil, false and "" are falsy.
func isTruthy(v interface{}) bool {
	switch val := v.(type) {
	case nil:
		return false
	case bool:
		return val
	case string:
		return val != ""
	default:
		return true
	}
}

// injectVarAnnotations copies the "var" and "namespace_suffix" fields
// from each sandbox resource into its annotations, matching the Python
// inject_var_annotations filter.
func injectVarAnnotations(sandboxes []interface{}) []interface{} {
	result := make([]interface{}, len(sandboxes))
	for i, s := range sandboxes {
		sb, ok := s.(map[string]interface{})
		if !ok {
			result[i] = s
			continue
		}
		result[i] = copyWithInjectedAnnotations(sb)
	}
	return result
}

// getStringDefault returns the string value of a key from a map,
// returning defaultVal if missing or not a string.
func getStringDefault(m map[string]interface{}, key, defaultVal string) string {
	v, ok := m[key]
	if !ok {
		return defaultVal
	}
	s, ok := v.(string)
	if !ok {
		return defaultVal
	}
	return s
}

// getCredList returns the credentials list from a resource map.
func getCredList(res map[string]interface{}) []map[string]interface{} {
	raw, ok := res["credentials"].([]interface{})
	if !ok {
		return nil
	}
	var creds []map[string]interface{}
	for _, c := range raw {
		if cm, ok := c.(map[string]interface{}); ok {
			creds = append(creds, cm)
		}
	}
	return creds
}

// extractAwsSandboxVars extracts vars from an AwsSandbox resource.
func extractAwsSandboxVars(res map[string]interface{}, creds bool) map[string]interface{} {
	name := getStringDefault(res, "name", "unknown")
	hostedZoneID := getStringDefault(res, "hosted_zone_id", "unknown")
	accountID := getStringDefault(res, "account_id", "unknown")
	zone := getStringDefault(res, "zone", "unknown")

	toMerge := map[string]interface{}{
		"sandbox_name":           name,
		"sandbox_hosted_zone_id": hostedZoneID,
		"HostedZoneId":           hostedZoneID,
		"sandbox_account":        accountID,
		"sandbox_account_id":     accountID,
		"sandbox_zone":           zone,
		"subdomain_base_suffix":  "." + zone,
	}

	if creds {
		for _, cred := range getCredList(res) {
			if getStringDefault(cred, "kind", "none") == "aws_iam_key" {
				toMerge["sandbox_aws_access_key_id"] = getStringDefault(cred, "aws_access_key_id", "unknown")
				toMerge["aws_access_key_id"] = getStringDefault(cred, "aws_access_key_id", "unknown")
				toMerge["sandbox_aws_secret_access_key"] = getStringDefault(cred, "aws_secret_access_key", "unknown")
				toMerge["aws_secret_access_key"] = getStringDefault(cred, "aws_secret_access_key", "unknown")
				// Include raw credentials list.
				rawCreds, _ := res["credentials"].([]interface{})
				toMerge["sandbox_credentials"] = rawCreds
				break
			}
		}
	}

	return toMerge
}

// extractOcpSandboxVars extracts vars from an OcpSandbox resource.
func extractOcpSandboxVars(res map[string]interface{}, creds bool) map[string]interface{} {
	toMerge := map[string]interface{}{
		"sandbox_openshift_name":        getStringDefault(res, "name", "unknown"),
		"sandbox_openshift_namespace":   getStringDefault(res, "namespace", "unknown"),
		"sandbox_openshift_cluster":     getStringDefault(res, "ocp_cluster", "unknown"),
		"sandbox_openshift_api_url":     getStringDefault(res, "api_url", "unknown"),
		"sandbox_openshift_apps_domain": getStringDefault(res, "ingress_domain", "unknown"),
		"sandbox_openshift_console_url": getStringDefault(res, "console_url", "unknown"),
	}

	if creds {
		for _, cred := range getCredList(res) {
			credKind := getStringDefault(cred, "kind", "none")
			if credKind == "ServiceAccount" {
				toMerge["sandbox_openshift_api_key"] = getStringDefault(cred, "token", "unknown")
				toMerge["sandbox_openshift_api_token"] = getStringDefault(cred, "token", "unknown")
				rawCreds, _ := res["credentials"].([]interface{})
				toMerge["sandbox_openshift_credentials"] = rawCreds
			}
			if credKind == "KeycloakUser" {
				toMerge["sandbox_openshift_user"] = getStringDefault(cred, "username", "unknown")
				toMerge["sandbox_openshift_password"] = getStringDefault(cred, "password", "unknown")
			}
		}
	}

	// Merge additional vars from cluster_additional_vars or additional_vars.
	additionalVars := types.GetNestedMap(res, "cluster_additional_vars", "deployer")
	if additionalVars == nil {
		additionalVars = types.GetNestedMap(res, "additional_vars", "deployer")
	}
	for k, v := range additionalVars {
		toMerge[k] = v
	}

	return toMerge
}

// extractIBMSandboxVars extracts vars from an IBMResourceGroupSandbox resource.
func extractIBMSandboxVars(res map[string]interface{}, creds bool) map[string]interface{} {
	toMerge := make(map[string]interface{})

	if creds {
		credList := getCredList(res)
		for _, cred := range credList {
			if getStringDefault(cred, "apikey", "") != "" {
				// Use first credential entry's apikey and name.
				if len(credList) > 0 {
					toMerge["ibmcloud_api_key"] = getStringDefault(credList[0], "apikey", "unknown")
					toMerge["ibmcloud_resource_group_name"] = getStringDefault(credList[0], "name", "unknown")
				}
				break
			}
		}
	}

	// Merge additional vars.
	additionalVars := types.GetNestedMap(res, "additional_vars", "deployer")
	for k, v := range additionalVars {
		toMerge[k] = v
	}

	return toMerge
}

// extractRosaSandboxVars extracts vars from a RosaSandbox resource.
// Unlike other kinds, credential fields are top-level (not in a credentials array).
func extractRosaSandboxVars(res map[string]interface{}, creds bool) map[string]interface{} {
	toMerge := map[string]interface{}{
		"rosa_sandbox_name":     getStringDefault(res, "name", "unknown"),
		"rosa_aws_account_name": getStringDefault(res, "aws_account_name", "unknown"),
	}

	if creds {
		toMerge["rosa_sa_client_id"] = getStringDefault(res, "sa_client_id", "unknown")
		toMerge["rosa_sa_secret"] = getStringDefault(res, "sa_secret", "unknown")
	}

	return toMerge
}

// resolveVarName returns the target variable name for a sandbox resource,
// taken from the "var" annotation. Defaults to "main" if not set.
func resolveVarName(res map[string]interface{}) string {
	if annotations, ok := res["annotations"].(map[string]interface{}); ok {
		if v, ok := annotations["var"].(string); ok && v != "" {
			return v
		}
	}
	return "main"
}

// extractResourceVars dispatches to the appropriate per-kind extractor
// for a single sandbox resource.
func extractResourceVars(res map[string]interface{}, creds bool) map[string]interface{} {
	kind := getStringDefault(res, "kind", "none")
	switch kind {
	case "AwsSandbox":
		return extractAwsSandboxVars(res, creds)
	case "OcpSandbox":
		return extractOcpSandboxVars(res, creds)
	case "IBMResourceGroupSandbox":
		return extractIBMSandboxVars(res, creds)
	case "RosaSandbox":
		return extractRosaSandboxVars(res, creds)
	default:
		toMerge := make(map[string]interface{})
		if creds {
			if rawCreds, _ := res["credentials"].([]interface{}); rawCreds != nil {
				toMerge["credentials"] = rawCreds
			}
		}
		return toMerge
	}
}

// extractSandboxVars extracts dynamic job variables from a placement
// response, matching the Python extract_sandboxes_vars filter.
// When creds is true, credential data is included (for Tower extra_vars).
// When creds is false, only non-secret data is included (for subject job_vars).
func extractSandboxVars(placement map[string]interface{}, creds bool) map[string]interface{} {
	vars := make(map[string]interface{})

	resources, ok := placement["resources"].([]interface{})
	if !ok || len(resources) == 0 {
		return vars
	}

	for _, r := range resources {
		res, ok := r.(map[string]interface{})
		if !ok {
			continue
		}

		toMerge := extractResourceVars(res, creds)
		varName := resolveVarName(res)

		if varName == "main" {
			types.DeepMergeMap(vars, toMerge)
		} else {
			vars[varName] = toMerge
		}
	}

	// Add full sandboxes response for downstream use when creds included.
	if creds {
		vars["sandboxes"] = types.DeepCopySlice(resources)
	}

	return vars
}

// extractSandboxLabels extracts labels from a placement response,
// matching the Python extract_sandboxes_labels filter.
func extractSandboxLabels(placement map[string]interface{}) map[string]string {
	resources, ok := placement["resources"].([]interface{})
	if !ok || len(resources) == 0 {
		return map[string]string{}
	}

	if len(resources) == 1 {
		res, ok := resources[0].(map[string]interface{})
		if !ok {
			return map[string]string{}
		}
		kind := sanitizeKind(getStringDefault(res, "kind", "none"))
		name := getStringDefault(res, "name", "unknown")
		return map[string]string{
			"sandbox": name,
			kind:      name,
		}
	}

	ret := make(map[string]string)
	increment := 2
	for _, r := range resources {
		res, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		kind := sanitizeKind(getStringDefault(res, "kind", "none"))
		name := getStringDefault(res, "name", "unknown")

		if ret["sandbox"] == "" {
			ret["sandbox"] = name
		}

		if _, exists := ret[kind]; exists {
			ret[fmt.Sprintf("%s%d", kind, increment)] = name
			increment++
		} else {
			ret[kind] = name
		}
	}

	return ret
}

// sanitizeKind removes non-alphanumeric characters from a kind string.
func sanitizeKind(kind string) string {
	result := make([]byte, 0, len(kind))
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		}
	}
	return string(result)
}
