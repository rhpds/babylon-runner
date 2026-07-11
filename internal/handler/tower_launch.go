package handler

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/rhpds/babylon-runner/internal/clients"
	"github.com/rhpds/babylon-runner/internal/runner"
	"github.com/rhpds/babylon-runner/internal/types"
)

const lifecycleEntryPoint = "ansible/lifecycle_entry_point.yml"

// Default deployer entry points from babylon governor defaults/main.yaml.
var defaultEntryPoints = map[string]string{
	"provision": "ansible/main.yml",
	"destroy":   "ansible/destroy.yml",
	"start":     lifecycleEntryPoint,
	"stop":      lifecycleEntryPoint,
	"status":    lifecycleEntryPoint,
	"update":    lifecycleEntryPoint,
}

// getDeployerEntryPoint returns the playbook path for the given action.
// Lookup order (matching Ansible defaults/main.yaml):
//  1. __meta__.deployer.actions.{action}.entry_point
//  2. __meta__.deployer.entry_point (generic)
//  3. Per-action default from defaultEntryPoints
func getDeployerEntryPoint(meta *types.Meta, action string) string {
	if meta != nil && meta.Deployer != nil {
		if actionCfg, ok := meta.Deployer.Actions[action]; ok && actionCfg.EntryPoint != "" {
			return actionCfg.EntryPoint
		}
		if meta.Deployer.EntryPoint != "" {
			return meta.Deployer.EntryPoint
		}
	}
	if def, ok := defaultEntryPoints[action]; ok {
		return def
	}
	return "ansible/main.yml"
}

// getTowerClientForAction creates a TowerClient from the governor's
// __meta__.ansible_controllers configuration. Returns the client, the
// controller hostname, and any error.
func getTowerClientForAction(ctx context.Context, rc *runner.RunContext) (*clients.TowerClient, string, error) {
	if rc.TowerBaseURL != "" {
		return clients.NewTowerClient(rc.TowerBaseURL, "", "", rc.TowerTLSConfig), "test-tower", nil
	}

	meta := rc.Meta()
	if meta == nil {
		return nil, "", fmt.Errorf("no __meta__ in governor")
	}

	if meta.ControllerScheduler != nil && meta.ControllerScheduler.URL != "" {
		selected, hostname, err := trySchedulerSelection(ctx, rc, meta)
		if err != nil {
			slog.Warn("controller-scheduler failed, falling back to local selection",
				"error", err)
		} else {
			return selected, hostname, nil
		}
	}

	controllers := meta.AnsibleControllers
	if len(controllers) == 0 {
		return nil, "", fmt.Errorf("no ansible_controllers in __meta__")
	}

	mode := meta.AnsibleControllerSelectMode
	if mode == "" {
		mode = "random"
	}
	controller := clients.SelectController(controllers, mode)
	if controller == nil {
		return nil, "", fmt.Errorf("no controller selected")
	}

	hostname, _ := controller["hostname"].(string)
	if hostname == "" {
		return nil, "", fmt.Errorf("controller has no hostname")
	}

	username, password, err := resolveControllerCreds(rc, controller)
	if err != nil {
		return nil, "", fmt.Errorf("credentials for %s: %w", hostname, err)
	}

	return rc.TowerClientPool.Get(hostname, username, password, rc.TowerTLSConfig), hostname, nil
}

func trySchedulerSelection(ctx context.Context, rc *runner.RunContext, meta *types.Meta) (*clients.TowerClient, string, error) {
	cs := meta.ControllerScheduler

	apiKey, err := resolveSchedulerAPIKey(rc)
	if err != nil {
		return nil, "", fmt.Errorf("scheduler API key: %w", err)
	}

	scheduler := clients.NewSchedulerClient(cs.URL, apiKey, rc.TowerTLSConfig)
	resp, err := scheduler.Evaluate(ctx, clients.EvaluateRequest{
		RequireLabels: cs.RequireLabels,
		PreferLabels:  cs.PreferLabels,
		InstanceGroup: rc.ActionName(),
	})
	if err != nil {
		return nil, "", err
	}
	if len(resp.Ranked) == 0 {
		return nil, "", fmt.Errorf("scheduler returned empty ranking")
	}

	selectedHost := resp.Ranked[0].Domain
	slog.Info("controller-scheduler selected",
		"hostname", selectedHost,
		"score", resp.Ranked[0].Score)

	username, password, err := resolveControllerCreds(rc, map[string]interface{}{
		"hostname": selectedHost,
	})
	if err != nil {
		return nil, "", fmt.Errorf("credentials for scheduler-selected %s: %w", selectedHost, err)
	}

	return rc.TowerClientPool.Get(selectedHost, username, password, rc.TowerTLSConfig), selectedHost, nil
}

func resolveSchedulerAPIKey(rc *runner.RunContext) (string, error) {
	creds, _ := rc.Payload.Governor.Spec.Vars.Get("controller_scheduler_credentials").(map[string]interface{})
	if creds == nil {
		return "", fmt.Errorf("controller_scheduler_credentials not found in governor vars")
	}
	key, _ := creds["cluster_scheduler_api_key_governor"].(string)
	if key == "" {
		return "", fmt.Errorf("cluster_scheduler_api_key_governor is empty in controller_scheduler_credentials")
	}
	return key, nil
}

// resolveControllerCreds returns (username, password) for a controller by
// looking up the K8s Secret labeled babylon.gpte.redhat.com/ansible-control-plane={hostname}
// from the informer cache.
func resolveControllerCreds(rc *runner.RunContext, controller map[string]interface{}) (string, string, error) {
	hostname, _ := controller["hostname"].(string)
	if hostname == "" {
		return "", "", fmt.Errorf("controller has no hostname")
	}

	if rc.SecretCache == nil {
		return "", "", fmt.Errorf("secret cache not available, cannot resolve credentials for %s", hostname)
	}

	secret, ok := rc.SecretCache.GetByLabel(
		"babylon.gpte.redhat.com/ansible-control-plane", hostname)
	if !ok {
		return "", "", fmt.Errorf("no k8s secret found for controller %s", hostname)
	}

	username := string(secret.Data["user"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return "", "", fmt.Errorf("k8s secret for %s missing user or password", hostname)
	}
	return username, password, nil
}

// resolveActionExtraVars gets action-specific extra_vars from the raw
// governor __meta__.deployer.actions.<action>.extra_vars. Falls back to
// {"ACTION": action} if none are configured.
func resolveActionExtraVars(rc *runner.RunContext, action string) map[string]interface{} {
	govAllVars := rc.GovernorAllVars()
	rawMeta := types.GetNestedMap(govAllVars, "job_vars", "__meta__")
	actionExtraVars := types.GetNestedMap(rawMeta, "deployer", "actions", action, "extra_vars")
	if actionExtraVars != nil {
		return actionExtraVars
	}
	return map[string]interface{}{"ACTION": action}
}

// resolveCallbackVars returns the callback URL and token variable names
// from __meta__.deployer.callback_url_var and callback_token_var.
// Defaults to "agnosticd_callback_url" and "agnosticd_callback_token".
func resolveCallbackVars(rc *runner.RunContext) (urlVar, tokenVar string) {
	urlVar = "agnosticd_callback_url"
	tokenVar = "agnosticd_callback_token"
	govAllVars := rc.GovernorAllVars()
	rawMeta := types.GetNestedMap(govAllVars, "job_vars", "__meta__")
	rawDeployer := types.GetNestedMap(rawMeta, "deployer")
	if rawDeployer == nil {
		return urlVar, tokenVar
	}
	if v, ok := rawDeployer["callback_url_var"].(string); ok && v != "" {
		urlVar = v
	}
	if v, ok := rawDeployer["callback_token_var"].(string); ok && v != "" {
		tokenVar = v
	}
	return urlVar, tokenVar
}

// resolveDeployerType returns the deployer type from meta, defaulting
// to "agnosticd".
func resolveDeployerType(meta *types.Meta) string {
	if meta != nil && meta.Deployer != nil && meta.Deployer.Type != "" {
		return meta.Deployer.Type
	}
	return "agnosticd"
}

// injectSCMRefVar sets the SCM ref variable in extraVars if
// __meta__.deployer.scm_ref_var is configured and meta.Deployer.SCMRef
// is non-empty.
func injectSCMRefVar(rc *runner.RunContext, meta *types.Meta, extraVars map[string]interface{}) {
	govAllVars := rc.GovernorAllVars()
	rawMeta := types.GetNestedMap(govAllVars, "job_vars", "__meta__")
	rawDeployer := types.GetNestedMap(rawMeta, "deployer")
	if rawDeployer == nil {
		return
	}
	scmRefVar, ok := rawDeployer["scm_ref_var"].(string)
	if !ok || scmRefVar == "" {
		return
	}
	if meta != nil && meta.Deployer != nil && meta.Deployer.SCMRef != "" {
		extraVars[scmRefVar] = meta.Deployer.SCMRef
	}
}

// buildJobExtraVars assembles the extra variables for a Tower job by
// merging subject job_vars, governor job_vars, and dynamic vars (sandbox
// creds), then applying action-specific overrides and callback vars.
// This matches the Ansible assembly order:
//
//	subject | combine(governor) | combine(dynamic) | remove_keys(__meta__)
//	| combine(action_extra_vars) | combine(callback_vars)
//	| combine(output_dir) | combine(scm_ref_var)
func buildJobExtraVars(rc *runner.RunContext, action string, dynamicJobVars map[string]interface{}) map[string]interface{} {
	extraVars := make(map[string]interface{})

	// Subject first (lowest priority).
	if sjv := rc.JobVars(); sjv != nil {
		types.DeepMergeMap(extraVars, sjv)
	}
	// Governor overrides subject.
	if gjv := rc.GovernorJobVars(); gjv != nil {
		types.DeepMergeMap(extraVars, gjv)
	}
	// Dynamic vars (sandbox creds) override both.
	if dynamicJobVars != nil {
		types.DeepMergeMap(extraVars, dynamicJobVars)
	}

	slog.Debug("buildJobExtraVars sources",
		"subjectJobVarsNil", rc.JobVars() == nil,
		"governorJobVarsNil", rc.GovernorJobVars() == nil,
		"dynamicJobVarsNil", dynamicJobVars == nil)

	// Remove __meta__ from merged vars.
	delete(extraVars, "__meta__")

	keys := make([]string, 0, len(extraVars))
	for k := range extraVars {
		keys = append(keys, k)
	}
	slog.Debug("buildJobExtraVars", "numVars", len(extraVars), "keys", keys)

	// Action extra_vars from deployer config, defaulting to {"ACTION": action}.
	types.DeepMergeMap(extraVars, resolveActionExtraVars(rc, action))

	// Callback vars with configurable var names.
	callbackURLVar, callbackTokenVar := resolveCallbackVars(rc)
	if rc.Payload.Action != nil {
		if rc.Payload.Action.Spec.CallbackUrl != "" {
			extraVars[callbackURLVar] = rc.Payload.Action.Spec.CallbackUrl
		}
		if rc.Payload.Action.Spec.CallbackToken != "" {
			extraVars[callbackTokenVar] = rc.Payload.Action.Spec.CallbackToken
		}
	}

	// output_dir if deployer_type is "agnosticd" (default).
	meta := rc.Meta()
	if resolveDeployerType(meta) == "agnosticd" {
		if uuid := rc.UUID(); uuid != "" {
			extraVars["output_dir"] = "/tmp/output-" + uuid
		}
	}

	// scm_ref_var injection.
	injectSCMRefVar(rc, meta, extraVars)

	return extraVars
}

// getTowerClientForHost creates a TowerClient for a specific controller
// hostname by finding the matching entry in __meta__.ansible_controllers.
// This is used by checkDeployerJob and cancelTowerJob to connect to the
// same controller where the job was originally launched.
func getTowerClientForHost(rc *runner.RunContext, hostname string) (*clients.TowerClient, error) {
	if rc.TowerBaseURL != "" {
		return clients.NewTowerClient(rc.TowerBaseURL, "", "", rc.TowerTLSConfig), nil
	}

	meta := rc.Meta()
	if meta == nil {
		return nil, fmt.Errorf("no __meta__ in governor")
	}

	controllers := meta.AnsibleControllers
	if len(controllers) == 0 {
		return nil, fmt.Errorf("no ansible_controllers in __meta__")
	}

	for _, m := range controllers {
		h, _ := m["hostname"].(string)
		if h == hostname {
			username, password, err := resolveControllerCreds(rc, m)
			if err != nil {
				return nil, fmt.Errorf("credentials for %s: %w", hostname, err)
			}
			return rc.TowerClientPool.Get(hostname, username, password, rc.TowerTLSConfig), nil
		}
	}

	return nil, fmt.Errorf("controller %q not found in ansible_controllers", hostname)
}

// resolveTowerOrg returns the Tower organization from meta, defaulting
// to "babylon".
func resolveTowerOrg(meta *types.Meta) string {
	if meta != nil && meta.Tower != nil && meta.Tower.Organization != "" {
		return meta.Tower.Organization
	}
	return "babylon"
}

// resolveTowerTimeout returns the Tower job timeout from meta, defaulting
// to 10800 seconds (3 hours).
func resolveTowerTimeout(meta *types.Meta) int {
	if meta != nil && meta.Tower != nil && meta.Tower.Timeout > 0 {
		return meta.Tower.Timeout
	}
	return 10800
}

// resolveTowerInventory returns the Tower inventory name as
// "{org} {suffix}" where suffix comes from meta or defaults to "default".
func resolveTowerInventory(meta *types.Meta, org string) string {
	suffix := "default"
	if meta != nil && meta.Tower != nil && meta.Tower.Inventory != "" {
		suffix = meta.Tower.Inventory
	}
	return org + " " + suffix
}

// resolveSCMSettings returns SCM settings for the Tower project.
// updateOnLaunch is false if scmRef matches a release version pattern
// (digits.digits). cacheTimeout defaults to 30, clean defaults to true.
func resolveSCMSettings(meta *types.Meta, scmRef string) (updateOnLaunch bool, cacheTimeout int, clean bool) {
	updateOnLaunch = !regexp.MustCompile(`\d+\.\d+$`).MatchString(scmRef)
	cacheTimeout = 30
	if meta != nil && meta.Deployer != nil && meta.Deployer.SCMCacheTimeout > 0 {
		cacheTimeout = meta.Deployer.SCMCacheTimeout
	}
	clean = true
	if meta != nil && meta.Deployer != nil && meta.Deployer.SCMClean != nil {
		clean = *meta.Deployer.SCMClean
	}
	return updateOnLaunch, cacheTimeout, clean
}

// buildEEConfig builds the execution environment configuration from the
// raw governor vars. Returns nil if the control plane type is not
// "controller" or no EE image is configured.
func buildEEConfig(rc *runner.RunContext, org string) *clients.EEConfig {
	govAllVars := rc.GovernorAllVars()
	rawMeta := types.GetNestedMap(govAllVars, "job_vars", "__meta__")

	controlPlaneType := types.FirstString(
		types.StringFromMap(types.GetNestedMap(rawMeta, "ansible_control_plane"), "type"),
		"tower",
	)
	if controlPlaneType != "controller" {
		return nil
	}

	rawDeployer := types.GetNestedMap(rawMeta, "deployer")
	ee := types.GetNestedMap(rawDeployer, "execution_environment")
	if ee == nil {
		return nil
	}

	eeImage := types.StringFromMap(ee, "image")
	if eeImage == "" {
		return nil
	}

	eeName := types.StringFromMap(ee, "name")
	if eeName == "" {
		eeName = org + " " + eeImage
	}
	eePull, _ := ee["pull"].(string)
	eePrivate, _ := ee["private"].(bool)

	return &clients.EEConfig{
		Name:    eeName,
		Image:   eeImage,
		Pull:    eePull,
		Private: eePrivate,
	}
}

// resolveInstanceGroups returns instance group names for the Tower job.
// Checks action-specific override first, then falls back to global
// ansible_control_plane.instance_groups.
func resolveInstanceGroups(rc *runner.RunContext, action string) []string {
	govAllVars := rc.GovernorAllVars()
	rawMeta := types.GetNestedMap(govAllVars, "job_vars", "__meta__")
	rawDeployer := types.GetNestedMap(rawMeta, "deployer")

	// Per-action override first.
	actionACP := types.GetNestedMap(rawDeployer, "actions", action, "ansible_control_plane")
	if groups := types.ExtractStringSlice(actionACP, "instance_groups"); len(groups) > 0 {
		return groups
	}

	// Global fallback.
	acp := types.GetNestedMap(rawMeta, "ansible_control_plane")
	return types.ExtractStringSlice(acp, "instance_groups")
}

// resolveStaticCredentials returns credential names from
// __meta__.deployer.credentials.
func resolveStaticCredentials(rc *runner.RunContext) []string {
	govAllVars := rc.GovernorAllVars()
	rawMeta := types.GetNestedMap(govAllVars, "job_vars", "__meta__")
	rawDeployer := types.GetNestedMap(rawMeta, "deployer")
	return types.ExtractStringSlice(rawDeployer, "credentials")
}

// resolveVaultCredentials returns vault credentials from
// governor.spec.vars.vault_credentials as a map of vault_id to password.
func resolveVaultCredentials(rc *runner.RunContext) map[string]string {
	vcRaw, _ := rc.Payload.Governor.Spec.Vars.Get("vault_credentials").(map[string]interface{})
	if len(vcRaw) == 0 {
		return nil
	}
	result := make(map[string]string, len(vcRaw))
	for k, v := range vcRaw {
		if s, ok := v.(string); ok {
			result[k] = s
		}
	}
	return result
}

// launchTowerJob selects a controller, builds the job config, launches
// the Tower job, and updates the subject status. If newState is non-empty,
// it also transitions labels and current_state. extraSpecVars are merged
// into the subject spec.vars patch (e.g. check_status_state for status).
// dynamicJobVars are sandbox vars with credentials, merged into Tower extra_vars.
func launchTowerJob(ctx context.Context, rc *runner.RunContext, action, newState string, extraSpecVars, dynamicJobVars map[string]interface{}) error {
	meta := rc.Meta()

	playbook := getDeployerEntryPoint(meta, action)
	scmURL := ""
	scmRef := ""
	if meta != nil && meta.Deployer != nil {
		scmURL = meta.Deployer.SCMUrl
		scmRef = meta.Deployer.SCMRef
	}

	tc, hostname, err := getTowerClientForAction(ctx, rc)
	if err != nil {
		return fmt.Errorf("get tower client: %w", err)
	}

	org := resolveTowerOrg(meta)
	scmUpdateOnLaunch, scmUpdateCacheTimeout, scmClean := resolveSCMSettings(meta, scmRef)

	// Template name: "{org} {anarchy_action_name} {uuid}".
	actionResourceName := ""
	if rc.Payload.Action != nil {
		actionResourceName = rc.Payload.Action.Metadata.Name
	}
	uuid := rc.UUID()

	config := clients.TowerJobConfig{
		Organization:          org,
		Inventory:             resolveTowerInventory(meta, org),
		ProjectName:           fmt.Sprintf("%s %s (%s)", org, scmURL, scmRef),
		ProjectSCMURL:         scmURL,
		ProjectSCMRef:         scmRef,
		SCMUpdateOnLaunch:     scmUpdateOnLaunch,
		SCMUpdateCacheTimeout: scmUpdateCacheTimeout,
		SCMClean:              scmClean,
		TemplateName:          fmt.Sprintf("%s %s %s", org, actionResourceName, uuid),
		Playbook:              playbook,
		ExtraVars:             clients.InsertUnvaultString(buildJobExtraVars(rc, action, dynamicJobVars)),
		Timeout:               resolveTowerTimeout(meta),
		ExecutionEnvironment:  buildEEConfig(rc, org),
		InstanceGroups:        resolveInstanceGroups(rc, action),
		Credentials:           resolveStaticCredentials(rc),
		VaultCredentials:      resolveVaultCredentials(rc),
	}

	slog.Info("launching tower job", "action", action, "subject", rc.SubjectName(), "entryPoint", playbook)

	jobID, err := tc.LaunchJob(ctx, config)
	if err != nil {
		return fmt.Errorf("launch tower job: %w", err)
	}

	slog.Info("tower job launched", "jobID", jobID, "action", action, "subject", rc.SubjectName())

	towerJobURL := fmt.Sprintf("%s/#/jobs/%d/", hostname, jobID)

	// Build the subject update patch.
	patchBody := types.PatchBody{
		Status: map[string]interface{}{
			"towerJobs": map[string]interface{}{
				action: map[string]interface{}{
					"deployerJob":      float64(jobID),
					"startTimestamp":    types.NowUTC(),
					"completeTimestamp": nil,
					"towerHost":        hostname,
					"towerJobURL":      towerJobURL,
				},
			},
		},
	}

	if newState != "" {
		patchBody.Metadata = &types.PatchMetadata{
			Labels: map[string]string{"state": newState},
		}
		patchBody.Spec = &types.PatchSpec{
			Vars: map[string]interface{}{
				"current_state": newState,
			},
		}
	}

	// Merge extra spec vars (e.g. check_status_state for status action).
	if len(extraSpecVars) > 0 {
		if patchBody.Spec == nil {
			patchBody.Spec = &types.PatchSpec{Vars: make(map[string]interface{})}
		}
		for k, v := range extraSpecVars {
			patchBody.Spec.Vars[k] = v
		}
	}

	return rc.SubjectUpdate(ctx, types.SubjectPatch{Patch: patchBody})
}

// cancelTowerJob cancels a running Tower job for the given action.
// Errors are logged but not returned to avoid failing the caller.
func cancelTowerJob(ctx context.Context, rc *runner.RunContext, action string) {
	towerJobs := rc.StatusTowerJobs()
	jobInfo := types.GetNestedMap(towerJobs, action)
	if jobInfo == nil {
		return
	}

	// Skip if already complete.
	if ct, ok := jobInfo["completeTimestamp"]; ok && ct != nil {
		return
	}

	jobIDFloat, ok := jobInfo["deployerJob"].(float64)
	if !ok || jobIDFloat == 0 {
		return
	}

	// Use towerHost from the job status to connect to the correct controller.
	towerHost, _ := jobInfo["towerHost"].(string)
	if towerHost == "" {
		slog.Warn("cancelTowerJob: no towerHost in job status", "action", action)
		return
	}

	tc, err := getTowerClientForHost(rc, towerHost)
	if err != nil {
		slog.Error("cancelTowerJob: cannot get tower client", "host", towerHost, "error", err)
		return
	}

	token, err := tc.GetToken(ctx)
	if err != nil {
		slog.Error("cancelTowerJob: cannot get oauth token", "error", err)
		return
	}

	if err := tc.CancelJob(ctx, token, int(jobIDFloat)); err != nil {
		slog.Error("cancelTowerJob: failed to cancel job", "job", int(jobIDFloat), "error", err)
	} else {
		slog.Info("cancelTowerJob: canceled job", "job", int(jobIDFloat), "action", action)
	}
}

// cancelAllIncompleteTowerJobs cancels every tower job in
// subject.status.towerJobs that has no completeTimestamp.
func cancelAllIncompleteTowerJobs(ctx context.Context, rc *runner.RunContext) {
	towerJobs := rc.StatusTowerJobs()
	if towerJobs == nil {
		return
	}
	for action, v := range towerJobs {
		if _, ok := v.(map[string]interface{}); !ok {
			continue
		}
		cancelTowerJob(ctx, rc, action)
	}
}

// extractProvisionData extracts provision-specific data from the Tower
// job response artifacts.
func extractProvisionData(jobStatus map[string]interface{}) (data, messageBody, messages interface{}) {
	artifacts, _ := jobStatus["artifacts"].(map[string]interface{})
	if artifacts != nil {
		data = artifacts["provision_data"]
		messageBody = artifacts["provision_message_body"]
		messages = artifacts["provision_messages"]
	}
	return data, messageBody, messages
}

