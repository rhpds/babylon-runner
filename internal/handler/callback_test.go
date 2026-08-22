package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/rhpds/babylon-runner/internal/runner"
	"github.com/rhpds/babylon-runner/internal/types"
)

// newCallbackRunContext builds a RunContext for a callback run, with the
// given handler name and optional callback payload data.
func newCallbackRunContext(t *testing.T, server *httptest.Server, callbackName string, callbackData map[string]interface{}) *runner.RunContext {
	t.Helper()

	rc := newTestRunContext(t, server)
	rc.Payload.Handler = types.Handler{
		Type: "actionCallback",
		Name: callbackName,
		Vars: map[string]interface{}{
			"anarchy_action_callback_data": callbackData,
		},
	}
	return rc
}

func TestCallbackPayloadData(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]interface{}
		wantData interface{}
		wantMsgB interface{}
		wantMsgs interface{}
	}{
		{name: "nil vars", vars: nil, wantData: nil, wantMsgB: nil, wantMsgs: nil},
		{name: "no callback data", vars: map[string]interface{}{}, wantData: nil, wantMsgB: nil, wantMsgs: nil},
		{
			name: "full callback data",
			vars: map[string]interface{}{
				"anarchy_action_callback_data": map[string]interface{}{
					"data":         map[string]interface{}{"foo": "bar"},
					"message_body": []interface{}{"hello"},
					"messages":     []interface{}{"a", "b"},
				},
			},
			wantData: map[string]interface{}{"foo": "bar"},
			wantMsgB: []interface{}{"hello"},
			wantMsgs: []interface{}{"a", "b"},
		},
		{
			name: "callback data not a map",
			vars: map[string]interface{}{
				"anarchy_action_callback_data": "just a string",
			},
			wantData: nil, wantMsgB: nil, wantMsgs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := &runner.RunContext{
				Payload: types.RunPayload{
					Handler: types.Handler{Vars: tt.vars},
				},
			}
			data, msgB, msgs := callbackPayloadData(rc)
			if !interfaceEqual(data, tt.wantData) {
				t.Errorf("data = %v, want %v", data, tt.wantData)
			}
			if !interfaceEqual(msgB, tt.wantMsgB) {
				t.Errorf("message_body = %v, want %v", msgB, tt.wantMsgB)
			}
			if !interfaceEqual(msgs, tt.wantMsgs) {
				t.Errorf("messages = %v, want %v", msgs, tt.wantMsgs)
			}
		})
	}
}

func TestHandleCallbackProvisionComplete(t *testing.T) {
	server, calls := newTestAnarchyServer(t)
	defer server.Close()

	provisionData := map[string]interface{}{"weburl": "https://example.com"}
	rc := newCallbackRunContext(t, server, "complete", map[string]interface{}{
		"data":         provisionData,
		"message_body": []interface{}{"provisioned"},
		"messages":     []interface{}{"done"},
	})
	rc.Payload.Subject.Spec.Vars.JobVars = map[string]interface{}{"uuid": "u1"}

	if err := handleCallback(context.Background(), rc, "provision", "complete"); err != nil {
		t.Fatalf("handleCallback returned error: %v", err)
	}

	// Must be one subject PATCH only (complete handlers do not continue/schedule).
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	c0 := (*calls)[0]
	if c0.Method != "PATCH" {
		t.Errorf("method = %s, want PATCH", c0.Method)
	}
	patch := c0.Body["patch"].(map[string]interface{})
	spec := patch["spec"].(map[string]interface{})
	vars := spec["vars"].(map[string]interface{})

	if vars["current_state"] != "started" {
		t.Errorf("current_state = %v, want started", vars["current_state"])
	}
	if vars["healthy"] != true {
		t.Errorf("healthy = %v, want true", vars["healthy"])
	}
	// Callback data must be propagated into the subject.
	pd, ok := vars["provision_data"].(map[string]interface{})
	if !ok || pd["weburl"] != "https://example.com" {
		t.Errorf("provision_data = %v, want %v", vars["provision_data"], provisionData)
	}

	// FinishAction should be "successful".
	if rc.Result.FinishAction == nil || rc.Result.FinishAction.State != "successful" {
		t.Errorf("FinishAction = %v, want successful", rc.Result.FinishAction)
	}
}

func TestHandleCallbackStatusComplete(t *testing.T) {
	server, calls := newTestAnarchyServer(t)
	defer server.Close()

	statusData := map[string]interface{}{"health": "ok"}
	rc := newCallbackRunContext(t, server, "complete", map[string]interface{}{
		"data":     statusData,
		"messages": []interface{}{"running"},
	})

	if err := handleCallback(context.Background(), rc, "status", "complete"); err != nil {
		t.Fatalf("handleCallback returned error: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	patch := (*calls)[0].Body["patch"].(map[string]interface{})
	vars := patch["spec"].(map[string]interface{})["vars"].(map[string]interface{})
	if vars["check_status_state"] != "successful" {
		t.Errorf("check_status_state = %v, want successful", vars["check_status_state"])
	}
	if vars["status_data"] == nil {
		t.Error("expected status_data to be set from callback payload")
	}

	if rc.Result.FinishAction == nil || rc.Result.FinishAction.State != "successful" {
		t.Errorf("FinishAction = %v, want successful", rc.Result.FinishAction)
	}
}

func TestHandleCallbackProvisionError(t *testing.T) {
	server, calls := newTestAnarchyServer(t)
	defer server.Close()

	rc := newCallbackRunContext(t, server, "error", map[string]interface{}{})
	rc.Payload.Subject.Spec.Vars.JobVars = map[string]interface{}{"uuid": "u1"}

	if err := handleCallback(context.Background(), rc, "provision", "error"); err != nil {
		t.Fatalf("handleCallback returned error: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	patch := (*calls)[0].Body["patch"].(map[string]interface{})
	vars := patch["spec"].(map[string]interface{})["vars"].(map[string]interface{})
	if vars["current_state"] != "provision-error" {
		t.Errorf("current_state = %v, want provision-error", vars["current_state"])
	}

	if rc.Result.FinishAction == nil || rc.Result.FinishAction.State != "error" {
		t.Errorf("FinishAction = %v, want error", rc.Result.FinishAction)
	}
}

func TestRegisterIncludesAllCallbacks(t *testing.T) {
	handlers := Register()

	expected := map[string]bool{}
	for _, action := range actionNames {
		for _, status := range callbackStatuses {
			expected["action:"+action+":"+status] = true
		}
	}

	// Every action callback must be registered.
	for key := range expected {
		if _, ok := handlers[key]; !ok {
			t.Errorf("Register() missing handler for %s", key)
		}
	}

	// No unexpected callback keys beyond the 24.
	for key := range handlers {
		if _, ok := expected[key]; ok {
			continue
		}
		if _, isEvent := map[string]bool{
			"event:create": true, "event:update": true, "event:delete": true,
		}[key]; isEvent {
			continue
		}
		if _, isAction := map[string]bool{
			"action:provision": true, "action:destroy": true, "action:start": true,
			"action:stop": true, "action:status": true, "action:update": true,
		}[key]; isAction {
			continue
		}
		t.Errorf("Register() has unexpected handler key %s", key)
	}
}
