package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rhpds/babylon-runner/internal/clients"
	"github.com/rhpds/babylon-runner/internal/runner"
	"github.com/rhpds/babylon-runner/internal/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// newSimpleSandboxServer creates a mock Sandbox API server with common routes.
func newSimpleSandboxServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for route, handler := range handlers {
			if strings.HasPrefix(r.URL.Path, route) || r.URL.Path == route {
				handler(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}))
}

// withSandboxEnabled configures a RunContext for sandbox API use.
func withSandboxEnabled(rc *runner.RunContext, sandboxServer *httptest.Server, uuid string) {
	rc.Payload.Governor.Spec.Vars.Meta = &types.Meta{AWSSandboxed: true}
	rc.Payload.Governor.Spec.Vars.SandboxAPI = map[string]interface{}{
		"sandbox_api_login_token": "test-login-token",
	}
	rc.Payload.Subject.Spec.Vars.JobVars = map[string]interface{}{
		"uuid": uuid,
		"guid": "test-guid-123",
	}
	rc.SandboxBaseURL = sandboxServer.URL
	rc.SandboxClientOpts = []clients.SandboxAPIOption{clients.WithNoRetries()}
}

// --- TestSandboxLoginToken ---

func TestSandboxLoginToken(t *testing.T) {
	tests := []struct {
		name      string
		setupRC   func(*runner.RunContext)
		wantToken string
	}{
		{
			name: "token present in sandbox_api",
			setupRC: func(rc *runner.RunContext) {
				rc.Payload.Governor.Spec.Vars.SandboxAPI = map[string]interface{}{
					"sandbox_api_login_token": "my-secret-token",
				}
			},
			wantToken: "my-secret-token",
		},
		{
			name:      "no sandbox_api - returns empty",
			setupRC:   func(rc *runner.RunContext) {},
			wantToken: "",
		},
		{
			name: "sandbox_api exists but no token - returns empty",
			setupRC: func(rc *runner.RunContext) {
				rc.Payload.Governor.Spec.Vars.SandboxAPI = map[string]interface{}{
					"some_other_field": "some-value",
				}
			},
			wantToken: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := newTestAnarchyServer(t)
			defer server.Close()

			rc := newTestRunContext(t, server)
			tt.setupRC(rc)

			token := sandboxLoginToken(rc)
			if token != tt.wantToken {
				t.Errorf("sandboxLoginToken() = %q, want %q", token, tt.wantToken)
			}
		})
	}
}

// --- TestGetSandboxClient ---

func TestGetSandboxClient(t *testing.T) {
	t.Run("success - creates client with login token", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		rc.Payload.Governor.Spec.Vars.SandboxAPI = map[string]interface{}{
			"sandbox_api_login_token": "test-login-token",
		}
		rc.SandboxBaseURL = "http://sandbox.example.com"
		rc.SandboxClientOpts = []clients.SandboxAPIOption{clients.WithNoRetries()}

		client, err := getSandboxClient(rc)
		if err != nil {
			t.Fatalf("getSandboxClient() error = %v", err)
		}
		if client == nil {
			t.Fatal("expected non-nil client")
		}
	})

	t.Run("no login token - returns error", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)

		_, err := getSandboxClient(rc)
		if err == nil {
			t.Fatal("expected error when no login token, got nil")
		}
		if !strings.Contains(err.Error(), "sandbox_api_login_token") {
			t.Errorf("error = %v, want error about sandbox_api_login_token", err)
		}
	})

	t.Run("URL from governor varSecret", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-123"})
			},
			"/api/v1/placements/uuid-1": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{"uuid": "uuid-1", "status": "ready"})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		rc.Payload.Governor.Spec.Vars.SandboxAPI = map[string]interface{}{
			"sandbox_api_login_token": "test-login-token",
			"sandbox_api_url":         sandboxServer.URL,
		}
		rc.SandboxClientOpts = []clients.SandboxAPIOption{clients.WithNoRetries()}

		client, err := getSandboxClient(rc)
		if err != nil {
			t.Fatalf("getSandboxClient() error = %v", err)
		}

		// Verify the client works by calling GetPlacement.
		result, statusCode, err := client.GetPlacement(context.Background(), "uuid-1")
		if err != nil {
			t.Fatalf("GetPlacement() error = %v", err)
		}
		if statusCode != 200 {
			t.Errorf("statusCode = %d, want 200", statusCode)
		}
		if result["uuid"] != "uuid-1" {
			t.Errorf("uuid = %v, want uuid-1", result["uuid"])
		}
	})
}

// --- TestSandboxGet ---

func TestSandboxGet(t *testing.T) {
	t.Run("no UUID - returns error", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)

		_, err := sandboxGet(context.Background(), rc, "provision")
		if err == nil {
			t.Fatal("expected error when no uuid, got nil")
		}
		if !strings.Contains(err.Error(), "no uuid") {
			t.Errorf("error = %v, want 'no uuid'", err)
		}
	})

	t.Run("placement found with resources - extracts vars and updates subject", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/": func(w http.ResponseWriter, r *http.Request) {
				placement := map[string]interface{}{
					"uuid":   "test-uuid-123",
					"status": "available",
					"resources": []interface{}{
						map[string]interface{}{
							"name": "sandbox-aws-1",
							"kind": "AwsSandbox",
							"credentials": []interface{}{
								map[string]interface{}{
									"kind":                  "aws_iam_key",
									"aws_access_key_id":     "AKIATEST",
									"aws_secret_access_key": "secret123",
								},
							},
							"hosted_zone_id": "Z1234567890ABC",
							"account_id":     "123456789012",
							"zone":           "sandbox.example.com",
						},
					},
				}
				json.NewEncoder(w).Encode(placement)
			},
		})
		defer sandboxServer.Close()

		anarchyServer, calls := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		result, err := sandboxGet(context.Background(), rc, "provision")
		if err != nil {
			t.Fatalf("sandboxGet() error = %v", err)
		}

		if result.Status != "success" {
			t.Errorf("Status = %s, want 'success'", result.Status)
		}

		// DynamicVars (creds=true) should have credential fields.
		if result.DynamicVars["aws_access_key_id"] != "AKIATEST" {
			t.Errorf("aws_access_key_id = %v, want 'AKIATEST'", result.DynamicVars["aws_access_key_id"])
		}
		if result.DynamicVars["sandbox_hosted_zone_id"] != "Z1234567890ABC" {
			t.Errorf("sandbox_hosted_zone_id = %v, want 'Z1234567890ABC'", result.DynamicVars["sandbox_hosted_zone_id"])
		}

		// Verify labels.
		if result.Labels["sandbox"] != "sandbox-aws-1" {
			t.Errorf("sandbox label = %s, want 'sandbox-aws-1'", result.Labels["sandbox"])
		}

		// Verify subject update was called.
		if len(*calls) == 0 {
			t.Fatal("expected at least one API call for subject update")
		}
		lastCall := (*calls)[len(*calls)-1]
		if lastCall.Method != http.MethodPatch {
			t.Errorf("last call method = %s, want PATCH", lastCall.Method)
		}
	})

	t.Run("placement not found + action=provision - calls book", func(t *testing.T) {
		sandboxServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/login":
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/placements/test-uuid-123":
				w.WriteHeader(http.StatusNotFound)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/placements":
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid":      "test-uuid-123",
					"status":    "available",
					"resources": []interface{}{},
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")
		// Set sandboxes in Meta (overwrite Meta but preserve AWSSandboxed).
		rc.Payload.Governor.Spec.Vars.Meta = &types.Meta{
			AWSSandboxed: true,
			Sandboxes: []interface{}{
				map[string]interface{}{"kind": "AwsSandbox"},
			},
		}

		result, err := sandboxGet(context.Background(), rc, "provision")
		if err != nil {
			t.Fatalf("sandboxGet() error = %v", err)
		}

		if result.Status != "success" {
			t.Errorf("Status = %s, want 'success'", result.Status)
		}
	})

	t.Run("placement not found + action=destroy - returns not-found", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		result, err := sandboxGet(context.Background(), rc, "destroy")
		if err != nil {
			t.Fatalf("sandboxGet() error = %v", err)
		}

		if result.Status != "not-found" {
			t.Errorf("Status = %s, want 'not-found'", result.Status)
		}
	})

	t.Run("placement status=error - returns error result", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid":   "test-uuid-123",
					"status": "error",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		result, err := sandboxGet(context.Background(), rc, "provision")
		if err != nil {
			t.Fatalf("sandboxGet() error = %v", err)
		}

		if result.Status != "error" {
			t.Errorf("Status = %s, want 'error'", result.Status)
		}
	})

	t.Run("placement status=queued - returns queued result", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid":   "test-uuid-123",
					"status": "queued",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		result, err := sandboxGet(context.Background(), rc, "provision")
		if err != nil {
			t.Fatalf("sandboxGet() error = %v", err)
		}

		if result.Status != "queued" {
			t.Errorf("Status = %s, want 'queued'", result.Status)
		}
	})
}

// --- TestValidateSandboxesRequest ---

// TestValidateSandboxesRequest mirrors the Python validate_sandboxes_request
// filter test cases (babylon.py).
func TestValidateSandboxesRequest(t *testing.T) {
	tests := []struct {
		name       string
		sandboxes  []interface{}
		wantResult string
	}{
		{
			name:       "empty request - error",
			sandboxes:  []interface{}{},
			wantResult: "ERROR: At least one sandbox is required in the sandboxes request",
		},
		{
			name: "single sandbox - OK",
			sandboxes: []interface{}{
				map[string]interface{}{"kind": "AwsSandbox"},
			},
			wantResult: "OK",
		},
		{
			name: "missing kind - error",
			sandboxes: []interface{}{
				map[string]interface{}{"var": "a"},
			},
			wantResult: "ERROR: Sandbox kind is required in the sandboxes request",
		},
		{
			name: "same kind with distinct vars - OK",
			sandboxes: []interface{}{
				map[string]interface{}{"kind": "OcpSandbox", "var": "main"},
				map[string]interface{}{"kind": "OcpSandbox", "var": "second"},
			},
			wantResult: "OK",
		},
		{
			name: "different kinds without vars - OK",
			sandboxes: []interface{}{
				map[string]interface{}{"kind": "AwsSandbox"},
				map[string]interface{}{"kind": "OcpSandbox"},
			},
			wantResult: "OK",
		},
		{
			name: "duplicated var - error",
			sandboxes: []interface{}{
				map[string]interface{}{"kind": "OcpSandbox", "var": "main"},
				map[string]interface{}{"kind": "AwsSandbox", "var": "main"},
			},
			wantResult: "ERROR: Variable 'main' is duplicated",
		},
		{
			name: "second sandbox of kind without var - error",
			sandboxes: []interface{}{
				map[string]interface{}{"kind": "AwsSandbox"},
				map[string]interface{}{"kind": "AwsSandbox"},
			},
			wantResult: "ERROR: missing 'var' key for second sandbox of kind AwsSandbox",
		},
		{
			name: "annotations of strings - OK",
			sandboxes: []interface{}{
				map[string]interface{}{
					"kind":        "AwsSandbox",
					"annotations": map[string]interface{}{"foo": "bar"},
				},
			},
			wantResult: "OK",
		},
		{
			name: "annotations not a dict - error",
			sandboxes: []interface{}{
				map[string]interface{}{
					"kind":        "AwsSandbox",
					"annotations": "not-a-dict",
				},
			},
			wantResult: "ERROR: Annotations should be a dict for sandbox of kind AwsSandbox",
		},
		{
			name: "annotations value not a string - error",
			sandboxes: []interface{}{
				map[string]interface{}{
					"kind":        "AwsSandbox",
					"annotations": map[string]interface{}{"foo": 42},
				},
			},
			wantResult: "ERROR: Annotations values should be strings for sandbox of kind AwsSandbox",
		},
		{
			name: "cloud_selector strings - OK",
			sandboxes: []interface{}{
				map[string]interface{}{
					"kind":           "AwsSandbox",
					"cloud_selector": map[string]interface{}{"version": "4.15"},
				},
			},
			wantResult: "OK",
		},
		{
			name: "cloud_selector booleans accepted - OK",
			sandboxes: []interface{}{
				map[string]interface{}{
					"kind":           "AwsSandbox",
					"cloud_selector": map[string]interface{}{"gpu": true},
				},
			},
			wantResult: "OK",
		},
		{
			name: "cloud_selector not a dict - error",
			sandboxes: []interface{}{
				map[string]interface{}{
					"kind":           "AwsSandbox",
					"cloud_selector": "not-a-dict",
				},
			},
			wantResult: "ERROR: cloud_selector should be a dict for sandbox of kind AwsSandbox",
		},
		{
			name: "cloud_preference value not string or bool - error",
			sandboxes: []interface{}{
				map[string]interface{}{
					"kind":             "AwsSandbox",
					"cloud_preference": map[string]interface{}{"version": 4.15},
				},
			},
			wantResult: "ERROR: cloud_preference values should be strings for sandbox of kind AwsSandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validateSandboxesRequest(tt.sandboxes); got != tt.wantResult {
				t.Errorf("validateSandboxesRequest() = %q, want %q", got, tt.wantResult)
			}
		})
	}
}

// --- TestSandboxBook ---

// newTestSandboxClient creates a SandboxAPIClient pointing at the given
// test server with retries disabled.
func newTestSandboxClient(serverURL string) *clients.SandboxAPIClient {
	return clients.NewSandboxAPIClient(serverURL, "test-login-token", clients.WithNoRetries())
}

func TestSandboxBook(t *testing.T) {
	t.Run("status 200 - success with extracted vars", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid":   "test-uuid-123",
					"status": "available",
					"resources": []interface{}{
						map[string]interface{}{
							"name": "sandbox-1",
							"kind": "AwsSandbox",
							"credentials": []interface{}{
								map[string]interface{}{
									"kind":                  "aws_iam_key",
									"aws_access_key_id":     "AKIA-BOOK",
									"aws_secret_access_key": "secret-book",
								},
							},
							"hosted_zone_id": "Z-BOOK",
							"account_id":     "999999999999",
							"zone":           "book.example.com",
						},
					},
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		client := newTestSandboxClient(sandboxServer.URL)
		result, err := sandboxBook(context.Background(), rc, client)
		if err != nil {
			t.Fatalf("sandboxBook() error = %v", err)
		}

		if result.Status != "success" {
			t.Errorf("Status = %s, want 'success'", result.Status)
		}
		if result.DynamicVars["aws_access_key_id"] != "AKIA-BOOK" {
			t.Errorf("aws_access_key_id = %v, want 'AKIA-BOOK'", result.DynamicVars["aws_access_key_id"])
		}
		if result.DynamicVars["sandbox_name"] != "sandbox-1" {
			t.Errorf("sandbox_name = %v, want 'sandbox-1'", result.DynamicVars["sandbox_name"])
		}
		if result.Labels["sandbox"] != "sandbox-1" {
			t.Errorf("sandbox label = %v, want 'sandbox-1'", result.Labels["sandbox"])
		}
	})

	t.Run("comment includes ocp_console_url", func(t *testing.T) {
		var gotBody map[string]interface{}
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid":   "test-uuid-123",
					"status": "available",
					"resources": []interface{}{
						map[string]interface{}{"name": "s1", "kind": "AwsSandbox"},
					},
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")
		rc.Clientset = fake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "console-public", Namespace: "openshift-config-managed"},
			Data:       map[string]string{"consoleURL": "https://console.example.com"},
		})

		client := newTestSandboxClient(sandboxServer.URL)
		if _, err := sandboxBook(context.Background(), rc, client); err != nil {
			t.Fatalf("sandboxBook() error = %v", err)
		}

		ann := gotBody["annotations"].(map[string]interface{})
		if ann["comment"] != "sandbox-api https://console.example.com" {
			t.Errorf("comment = %v, want 'sandbox-api https://console.example.com'", ann["comment"])
		}
	})

	t.Run("status 202 - queued", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid":   "test-uuid-123",
					"status": "queued",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		client := newTestSandboxClient(sandboxServer.URL)
		result, err := sandboxBook(context.Background(), rc, client)
		if err != nil {
			t.Fatalf("sandboxBook() error = %v", err)
		}

		if result.Status != "queued" {
			t.Errorf("Status = %s, want 'queued'", result.Status)
		}
	})

	t.Run("status 507 - queued (no capacity)", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(507)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"message": "No capacity",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		client := newTestSandboxClient(sandboxServer.URL)
		result, err := sandboxBook(context.Background(), rc, client)
		if err != nil {
			t.Fatalf("sandboxBook() error = %v", err)
		}

		if result.Status != "queued" {
			t.Errorf("Status = %s, want 'queued'", result.Status)
		}
	})

	t.Run("invalid sandboxes request - finishes action failed without booking", func(t *testing.T) {
		var bookCalled bool
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				bookCalled = true
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")
		rc.Payload.Governor.Spec.Vars.Meta = &types.Meta{
			AWSSandboxed: true,
			Sandboxes: []interface{}{
				map[string]interface{}{"kind": "AwsSandbox"},
				map[string]interface{}{"kind": "AwsSandbox"},
			},
		}

		client := newTestSandboxClient(sandboxServer.URL)
		result, err := sandboxBook(context.Background(), rc, client)
		if err != nil {
			t.Fatalf("sandboxBook() error = %v, want nil (failure is final, not retried)", err)
		}

		if result.Status != "invalid-request" {
			t.Errorf("Status = %s, want 'invalid-request'", result.Status)
		}
		if bookCalled {
			t.Error("expected no POST to /placements for an invalid request")
		}
		if rc.Result.FinishAction == nil {
			t.Fatal("expected FinishAction to be set")
		}
		if rc.Result.FinishAction.State != "failed" {
			t.Errorf("FinishAction state = %q, want 'failed'", rc.Result.FinishAction.State)
		}
	})

	t.Run("other status - error", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": "Bad request",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		client := newTestSandboxClient(sandboxServer.URL)
		result, err := sandboxBook(context.Background(), rc, client)
		if err == nil {
			t.Fatal("expected error for status 400, got nil")
		}

		if result.Status != "error" {
			t.Errorf("Status = %s, want 'error'", result.Status)
		}
	})

	t.Run("no __meta__.sandboxes - sends default AwsSandbox resource", func(t *testing.T) {
		var gotBody map[string]interface{}
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid":   "test-uuid-123",
					"status": "available",
					"resources": []interface{}{
						map[string]interface{}{"name": "s1", "kind": "AwsSandbox"},
					},
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123") // Meta.Sandboxes stays nil

		client := newTestSandboxClient(sandboxServer.URL)
		if _, err := sandboxBook(context.Background(), rc, client); err != nil {
			t.Fatalf("sandboxBook() error = %v", err)
		}

		resources, ok := gotBody["resources"].([]interface{})
		if !ok || len(resources) != 1 {
			t.Fatalf("resources = %#v, want 1-element slice", gotBody["resources"])
		}
		res := resources[0].(map[string]interface{})
		if res["kind"] != "AwsSandbox" {
			t.Errorf("resources[0].kind = %v, want AwsSandbox", res["kind"])
		}
		// JSON numbers decode to float64.
		if res["count"] != float64(1) {
			t.Errorf("resources[0].count = %v (%T), want 1", res["count"], res["count"])
		}
	})

	t.Run("explicit __meta__.sandboxes - default not applied", func(t *testing.T) {
		var gotBody map[string]interface{}
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uuid": "test-uuid-123", "status": "available",
					"resources": []interface{}{map[string]interface{}{"name": "s1", "kind": "OcpSandbox"}},
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")
		rc.Payload.Governor.Spec.Vars.Meta.Sandboxes = []interface{}{
			map[string]interface{}{"kind": "OcpSandbox", "namespace_suffix": "ocp4-cluster"},
		}

		client := newTestSandboxClient(sandboxServer.URL)
		if _, err := sandboxBook(context.Background(), rc, client); err != nil {
			t.Fatalf("sandboxBook() error = %v", err)
		}

		resources := gotBody["resources"].([]interface{})
		if len(resources) != 1 {
			t.Fatalf("resources len = %d, want 1", len(resources))
		}
		if resources[0].(map[string]interface{})["kind"] != "OcpSandbox" {
			t.Errorf("resources[0].kind = %v, want OcpSandbox", resources[0].(map[string]interface{})["kind"])
		}
	})

	t.Run("status 400 - error includes sandbox API body", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"http_code": 400,
					"message":   "Bad request: payload doesn't pass OpenAPI spec",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		client := newTestSandboxClient(sandboxServer.URL)
		_, err := sandboxBook(context.Background(), rc, client)
		if err == nil {
			t.Fatal("sandboxBook() error = nil, want error")
		}
		if !strings.Contains(err.Error(), "Bad request: payload doesn't pass OpenAPI spec") {
			t.Errorf("error = %q, want it to contain the sandbox API body", err.Error())
		}
	})

	t.Run("status 400 empty body - error has no null suffix", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest) // no body
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		client := newTestSandboxClient(sandboxServer.URL)
		_, err := sandboxBook(context.Background(), rc, client)
		if err == nil {
			t.Fatal("sandboxBook() error = nil, want error")
		}
		if strings.Contains(err.Error(), "null") {
			t.Errorf("error = %q, want no 'null' suffix for an empty body", err.Error())
		}
		if !strings.Contains(err.Error(), "status 400") {
			t.Errorf("error = %q, want it to contain the status code", err.Error())
		}
	})
}

// --- TestSandboxCleanup ---

func TestSandboxCleanup(t *testing.T) {
	t.Run("no UUID - returns error", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)

		err := sandboxCleanup(context.Background(), rc)
		if err == nil {
			t.Fatal("sandboxCleanup() error = nil, want error for missing uuid")
		}
	})

	t.Run("no GUID - returns error", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		rc.Payload.Subject.Spec.Vars.JobVars = map[string]interface{}{
			"uuid": "test-uuid",
		}

		err := sandboxCleanup(context.Background(), rc)
		if err == nil {
			t.Fatal("sandboxCleanup() error = nil, want error for missing guid")
		}
	})

	t.Run("no login token - skips without error", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		rc.Payload.Subject.Spec.Vars.JobVars = map[string]interface{}{
			"uuid": "test-uuid",
			"guid": "test-guid",
		}

		err := sandboxCleanup(context.Background(), rc)
		if err != nil {
			t.Fatalf("sandboxCleanup() error = %v, want nil", err)
		}
	})

	t.Run("success - releases placement", func(t *testing.T) {
		var deleteCalled bool
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/": func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleteCalled = true
					w.WriteHeader(http.StatusOK)
				}
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		err := sandboxCleanup(context.Background(), rc)
		if err != nil {
			t.Fatalf("sandboxCleanup() error = %v", err)
		}

		if !deleteCalled {
			t.Error("expected DELETE to be called")
		}
	})

	t.Run("release failure - returns error", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/": func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusInternalServerError)
				}
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		err := sandboxCleanup(context.Background(), rc)
		if err == nil {
			t.Fatal("expected error when release fails, got nil")
		}
		if !strings.Contains(err.Error(), "release placement") {
			t.Errorf("error = %v, want 'release placement' error", err)
		}
	})
}

func TestNormalizeBoolToYesNo(t *testing.T) {
	input := map[string]interface{}{
		"cloud_selector": map[string]interface{}{
			"cost_optimized": true,
			"region":         "us-east-1",
			"spot":           false,
		},
		"cloud_preference": map[string]interface{}{
			"balanced": true,
		},
		"unrelated": map[string]interface{}{
			"flag": true,
		},
	}

	normalizeBoolToYesNo(input, "cloud_selector")
	normalizeBoolToYesNo(input, "cloud_preference")

	cs := input["cloud_selector"].(map[string]interface{})
	if cs["cost_optimized"] != "yes" {
		t.Errorf("cost_optimized = %v, want 'yes'", cs["cost_optimized"])
	}
	if cs["spot"] != "no" {
		t.Errorf("spot = %v, want 'no'", cs["spot"])
	}
	if cs["region"] != "us-east-1" {
		t.Errorf("region = %v, want 'us-east-1' (unchanged)", cs["region"])
	}

	cp := input["cloud_preference"].(map[string]interface{})
	if cp["balanced"] != "yes" {
		t.Errorf("balanced = %v, want 'yes'", cp["balanced"])
	}

	// Unrelated map untouched.
	un := input["unrelated"].(map[string]interface{})
	if un["flag"] != true {
		t.Errorf("unrelated.flag = %v, want true (unchanged)", un["flag"])
	}
}

func TestInjectVarAnnotationsNormalizesCloudSelector(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"kind": "AwsSandbox",
			"var":  "sandbox2",
			"cloud_selector": map[string]interface{}{
				"cost_optimized": true,
			},
		},
	}
	out := injectVarAnnotations(input)

	res := out[0].(map[string]interface{})
	cs := res["cloud_selector"].(map[string]interface{})
	if cs["cost_optimized"] != "yes" {
		t.Errorf("cloud_selector.cost_optimized = %v, want 'yes'", cs["cost_optimized"])
	}
	if res["annotations"].(map[string]interface{})["var"] != "sandbox2" {
		t.Errorf("annotations.var = %v, want 'sandbox2'", res["annotations"])
	}
}

func TestOcpConsoleURL(t *testing.T) {
	anarchyServer, _ := newTestAnarchyServer(t)
	defer anarchyServer.Close()

	t.Run("nil clientset - returns empty", func(t *testing.T) {
		rc := newTestRunContext(t, anarchyServer)
		rc.Clientset = nil
		if got := ocpConsoleURL(context.Background(), rc); got != "" {
			t.Errorf("ocpConsoleURL() = %q, want empty", got)
		}
	})

	t.Run("with clientset but no configmap - returns empty", func(t *testing.T) {
		rc := newTestRunContext(t, anarchyServer)
		rc.Clientset = fake.NewSimpleClientset()
		if got := ocpConsoleURL(context.Background(), rc); got != "" {
			t.Errorf("ocpConsoleURL() = %q, want empty", got)
		}
	})

	t.Run("with console-public configmap - returns consoleURL", func(t *testing.T) {
		rc := newTestRunContext(t, anarchyServer)
		rc.Clientset = fake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "console-public",
				Namespace: "openshift-config-managed",
			},
			Data: map[string]string{"consoleURL": "https://console.example.com"},
		})
		if got := ocpConsoleURL(context.Background(), rc); got != "https://console.example.com" {
			t.Errorf("ocpConsoleURL() = %q, want console URL", got)
		}
	})
}

// --- TestSandboxStart ---

func TestSandboxStart(t *testing.T) {
	t.Run("no UUID - returns error", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)

		err := sandboxStart(context.Background(), rc)
		if err == nil {
			t.Fatal("expected error when no uuid, got nil")
		}
		if !strings.Contains(err.Error(), "no uuid") {
			t.Errorf("error = %v, want 'no uuid'", err)
		}
	})

	t.Run("success with request_id - polls and returns nil", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/test-uuid-123/start": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"request_id": "req-start-1",
				})
			},
			"/api/v1/requests/req-start-1/status": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "success",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		err := sandboxStart(context.Background(), rc)
		if err != nil {
			t.Fatalf("sandboxStart() error = %v", err)
		}
	})

	t.Run("no request_id in response - immediate success", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/test-uuid-123/start": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "started",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		err := sandboxStart(context.Background(), rc)
		if err != nil {
			t.Fatalf("sandboxStart() error = %v", err)
		}
	})

	t.Run("login failure - returns error", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		err := sandboxStart(context.Background(), rc)
		if err == nil {
			t.Fatal("expected error when login fails, got nil")
		}
	})
}

// --- TestSandboxStop ---

func TestSandboxStop(t *testing.T) {
	t.Run("no UUID - returns error", func(t *testing.T) {
		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)

		err := sandboxStop(context.Background(), rc)
		if err == nil {
			t.Fatal("expected error when no uuid, got nil")
		}
		if !strings.Contains(err.Error(), "no uuid") {
			t.Errorf("error = %v, want 'no uuid'", err)
		}
	})

	t.Run("success with request_id - polls and returns nil", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/placements/test-uuid-123/stop": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"request_id": "req-stop-1",
				})
			},
			"/api/v1/requests/req-stop-1/status": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "complete",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		err := sandboxStop(context.Background(), rc)
		if err != nil {
			t.Fatalf("sandboxStop() error = %v", err)
		}
	})

	t.Run("login failure - returns error", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

		err := sandboxStop(context.Background(), rc)
		if err == nil {
			t.Fatal("expected error when login fails, got nil")
		}
	})
}

// TestSandboxStopRecordsPrePollFailure verifies that a failed stop placement
// request still records jobStatus=error (with httpStatus and message) in
// subject.status.sandboxAPIJobs.stop, matching the Ansible
// sandbox_api_stop.yaml rescue block.
func TestSandboxStopRecordsPrePollFailure(t *testing.T) {
	sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
		"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
		},
		"/api/v1/placements/test-uuid-123/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	defer sandboxServer.Close()

	anarchyServer, calls := newTestAnarchyServer(t)
	defer anarchyServer.Close()

	rc := newTestRunContext(t, anarchyServer)
	withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

	err := sandboxStop(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error when stop placement fails, got nil")
	}

	var record map[string]interface{}
	for _, c := range *calls {
		if c.Method != http.MethodPatch {
			continue
		}
		patch, _ := c.Body["patch"].(map[string]interface{})
		status, _ := patch["status"].(map[string]interface{})
		jobs, _ := status["sandboxAPIJobs"].(map[string]interface{})
		if stop, _ := jobs["stop"].(map[string]interface{}); stop != nil {
			record = stop
		}
	}
	if record == nil {
		t.Fatal("expected sandboxAPIJobs.stop failure record in subject status")
	}
	if record["jobStatus"] != "error" {
		t.Errorf("jobStatus = %v, want error", record["jobStatus"])
	}
	if record["httpStatus"] != float64(http.StatusInternalServerError) {
		t.Errorf("httpStatus = %v, want %d", record["httpStatus"], http.StatusInternalServerError)
	}
	if msg, _ := record["message"].(string); !strings.Contains(msg, "stop placement") {
		t.Errorf("message = %q, want it to contain 'stop placement'", msg)
	}
}

// TestSandboxStartRecordsPrePollFailure is the start counterpart of
// TestSandboxStopRecordsPrePollFailure, matching the sandbox_api_start.yaml
// rescue block.
func TestSandboxStartRecordsPrePollFailure(t *testing.T) {
	sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
		"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
		},
		"/api/v1/placements/test-uuid-123/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	defer sandboxServer.Close()

	anarchyServer, calls := newTestAnarchyServer(t)
	defer anarchyServer.Close()

	rc := newTestRunContext(t, anarchyServer)
	withSandboxEnabled(rc, sandboxServer, "test-uuid-123")

	err := sandboxStart(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error when start placement fails, got nil")
	}

	var record map[string]interface{}
	for _, c := range *calls {
		if c.Method != http.MethodPatch {
			continue
		}
		patch, _ := c.Body["patch"].(map[string]interface{})
		status, _ := patch["status"].(map[string]interface{})
		jobs, _ := status["sandboxAPIJobs"].(map[string]interface{})
		if start, _ := jobs["start"].(map[string]interface{}); start != nil {
			record = start
		}
	}
	if record == nil {
		t.Fatal("expected sandboxAPIJobs.start failure record in subject status")
	}
	if record["jobStatus"] != "error" {
		t.Errorf("jobStatus = %v, want error", record["jobStatus"])
	}
	if record["httpStatus"] != float64(http.StatusInternalServerError) {
		t.Errorf("httpStatus = %v, want %d", record["httpStatus"], http.StatusInternalServerError)
	}
	if msg, _ := record["message"].(string); !strings.Contains(msg, "start placement") {
		t.Errorf("message = %q, want it to contain 'start placement'", msg)
	}
}

// --- TestPollSandboxRequest ---

func TestPollSandboxRequest(t *testing.T) {
	t.Run("status success - returns nil and records jobStatus", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/requests/req-1/status": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "success",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, calls := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		client := newTestSandboxClient(sandboxServer.URL)
		err := pollSandboxRequest(context.Background(), rc, client, "req-1", "start")
		if err != nil {
			t.Fatalf("pollSandboxRequest() error = %v", err)
		}
		if len(*calls) == 0 {
			t.Fatal("expected a subject PATCH recording the outcome")
		}
	})

	t.Run("status complete - returns nil and records jobStatus", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/requests/req-2/status": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "complete",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, calls := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		client := newTestSandboxClient(sandboxServer.URL)
		err := pollSandboxRequest(context.Background(), rc, client, "req-2", "start")
		if err != nil {
			t.Fatalf("pollSandboxRequest() error = %v", err)
		}
		if len(*calls) == 0 {
			t.Fatal("expected a subject PATCH recording the outcome")
		}
	})

	t.Run("status error - returns error with message", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/requests/req-3/status": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":  "error",
					"message": "Something went wrong",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		client := newTestSandboxClient(sandboxServer.URL)
		err := pollSandboxRequest(context.Background(), rc, client, "req-3", "start")
		if err == nil {
			t.Fatal("expected error for status 'error', got nil")
		}
		if !strings.Contains(err.Error(), "Something went wrong") {
			t.Errorf("error = %v, want 'Something went wrong'", err)
		}
	})

	t.Run("status failed - returns error", func(t *testing.T) {
		sandboxServer := newSimpleSandboxServer(t, map[string]http.HandlerFunc{
			"/api/v1/login": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
			},
			"/api/v1/requests/req-4/status": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":  "failed",
					"message": "Operation failed",
				})
			},
		})
		defer sandboxServer.Close()

		anarchyServer, _ := newTestAnarchyServer(t)
		defer anarchyServer.Close()

		rc := newTestRunContext(t, anarchyServer)
		client := newTestSandboxClient(sandboxServer.URL)
		err := pollSandboxRequest(context.Background(), rc, client, "req-4", "start")
		if err == nil {
			t.Fatal("expected error for status 'failed', got nil")
		}
		if !strings.Contains(err.Error(), "failed") {
			t.Errorf("error = %v, want 'failed' error", err)
		}
	})
}

// --- TestExtractSandboxVars ---

func TestExtractSandboxVars(t *testing.T) {
	t.Run("empty resources - empty vars", func(t *testing.T) {
		placement := map[string]interface{}{}
		vars := extractSandboxVars(placement, true)
		if len(vars) != 0 {
			t.Errorf("expected empty vars, got %d items", len(vars))
		}
	})

	t.Run("non-array resources - empty", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": "not-an-array",
		}
		vars := extractSandboxVars(placement, true)
		if len(vars) != 0 {
			t.Errorf("expected empty vars, got %d items", len(vars))
		}
	})

	t.Run("AwsSandbox with creds=true", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name":           "my-sandbox",
					"kind":           "AwsSandbox",
					"hosted_zone_id": "Z1234",
					"account_id":     "111111111111",
					"zone":           "sandbox123.example.com",
					"credentials": []interface{}{
						map[string]interface{}{
							"kind":                  "aws_iam_key",
							"aws_access_key_id":     "AKIA123",
							"aws_secret_access_key": "secret",
						},
					},
				},
			},
		}

		vars := extractSandboxVars(placement, true)

		if vars["sandbox_name"] != "my-sandbox" {
			t.Errorf("sandbox_name = %v, want my-sandbox", vars["sandbox_name"])
		}
		if vars["sandbox_hosted_zone_id"] != "Z1234" {
			t.Errorf("sandbox_hosted_zone_id = %v, want Z1234", vars["sandbox_hosted_zone_id"])
		}
		if vars["HostedZoneId"] != "Z1234" {
			t.Errorf("HostedZoneId = %v, want Z1234", vars["HostedZoneId"])
		}
		if vars["sandbox_account"] != "111111111111" {
			t.Errorf("sandbox_account = %v, want 111111111111", vars["sandbox_account"])
		}
		if vars["sandbox_account_id"] != "111111111111" {
			t.Errorf("sandbox_account_id = %v, want 111111111111", vars["sandbox_account_id"])
		}
		if vars["sandbox_zone"] != "sandbox123.example.com" {
			t.Errorf("sandbox_zone = %v, want sandbox123.example.com", vars["sandbox_zone"])
		}
		if vars["subdomain_base_suffix"] != ".sandbox123.example.com" {
			t.Errorf("subdomain_base_suffix = %v, want .sandbox123.example.com", vars["subdomain_base_suffix"])
		}
		if vars["aws_access_key_id"] != "AKIA123" {
			t.Errorf("aws_access_key_id = %v, want AKIA123", vars["aws_access_key_id"])
		}
		if vars["sandbox_aws_access_key_id"] != "AKIA123" {
			t.Errorf("sandbox_aws_access_key_id = %v, want AKIA123", vars["sandbox_aws_access_key_id"])
		}
		if vars["aws_secret_access_key"] != "secret" {
			t.Errorf("aws_secret_access_key = %v, want secret", vars["aws_secret_access_key"])
		}
		if vars["sandbox_aws_secret_access_key"] != "secret" {
			t.Errorf("sandbox_aws_secret_access_key = %v, want secret", vars["sandbox_aws_secret_access_key"])
		}
		// sandboxes deep copy included with creds=true
		sandboxes, ok := vars["sandboxes"].([]interface{})
		if !ok || len(sandboxes) != 1 {
			t.Errorf("expected sandboxes with 1 element, got %v", vars["sandboxes"])
		}
	})

	t.Run("AwsSandbox with creds=false - no credentials", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name":           "my-sandbox",
					"kind":           "AwsSandbox",
					"hosted_zone_id": "Z1234",
					"account_id":     "111111111111",
					"zone":           "sandbox123.example.com",
					"credentials": []interface{}{
						map[string]interface{}{
							"kind":                  "aws_iam_key",
							"aws_access_key_id":     "AKIA123",
							"aws_secret_access_key": "secret",
						},
					},
				},
			},
		}

		vars := extractSandboxVars(placement, false)

		// Non-cred fields still present.
		if vars["sandbox_name"] != "my-sandbox" {
			t.Errorf("sandbox_name = %v, want my-sandbox", vars["sandbox_name"])
		}
		if vars["sandbox_zone"] != "sandbox123.example.com" {
			t.Errorf("sandbox_zone = %v, want sandbox123.example.com", vars["sandbox_zone"])
		}
		// Cred fields must be absent.
		if _, ok := vars["aws_access_key_id"]; ok {
			t.Error("aws_access_key_id should not be present with creds=false")
		}
		if _, ok := vars["sandbox_aws_secret_access_key"]; ok {
			t.Error("sandbox_aws_secret_access_key should not be present with creds=false")
		}
		// No sandboxes deep copy without creds.
		if _, ok := vars["sandboxes"]; ok {
			t.Error("sandboxes should not be present with creds=false")
		}
	})

	t.Run("OcpSandbox with creds=true", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name":           "ocp-ns",
					"kind":           "OcpSandbox",
					"namespace":      "user-ns",
					"ocp_cluster":    "cluster1",
					"api_url":        "https://api.cluster1.example.com:6443",
					"ingress_domain": "apps.cluster1.example.com",
					"console_url":    "https://console.cluster1.example.com",
					"credentials": []interface{}{
						map[string]interface{}{
							"kind":  "ServiceAccount",
							"token": "sa-token-123",
						},
						map[string]interface{}{
							"kind":     "KeycloakUser",
							"username": "user1",
							"password": "pass1",
						},
					},
					"cluster_additional_vars": map[string]interface{}{
						"deployer": map[string]interface{}{
							"ocp4_workload_custom_var": "value1",
						},
					},
				},
			},
		}

		vars := extractSandboxVars(placement, true)

		if vars["sandbox_openshift_name"] != "ocp-ns" {
			t.Errorf("sandbox_openshift_name = %v, want ocp-ns", vars["sandbox_openshift_name"])
		}
		if vars["sandbox_openshift_namespace"] != "user-ns" {
			t.Errorf("sandbox_openshift_namespace = %v, want user-ns", vars["sandbox_openshift_namespace"])
		}
		if vars["sandbox_openshift_cluster"] != "cluster1" {
			t.Errorf("sandbox_openshift_cluster = %v, want cluster1", vars["sandbox_openshift_cluster"])
		}
		if vars["sandbox_openshift_api_url"] != "https://api.cluster1.example.com:6443" {
			t.Errorf("sandbox_openshift_api_url = %v", vars["sandbox_openshift_api_url"])
		}
		if vars["sandbox_openshift_apps_domain"] != "apps.cluster1.example.com" {
			t.Errorf("sandbox_openshift_apps_domain = %v", vars["sandbox_openshift_apps_domain"])
		}
		if vars["sandbox_openshift_console_url"] != "https://console.cluster1.example.com" {
			t.Errorf("sandbox_openshift_console_url = %v", vars["sandbox_openshift_console_url"])
		}
		if vars["sandbox_openshift_api_key"] != "sa-token-123" {
			t.Errorf("sandbox_openshift_api_key = %v, want sa-token-123", vars["sandbox_openshift_api_key"])
		}
		if vars["sandbox_openshift_api_token"] != "sa-token-123" {
			t.Errorf("sandbox_openshift_api_token = %v, want sa-token-123", vars["sandbox_openshift_api_token"])
		}
		if vars["sandbox_openshift_user"] != "user1" {
			t.Errorf("sandbox_openshift_user = %v, want user1", vars["sandbox_openshift_user"])
		}
		if vars["sandbox_openshift_password"] != "pass1" {
			t.Errorf("sandbox_openshift_password = %v, want pass1", vars["sandbox_openshift_password"])
		}
		// cluster_additional_vars.deployer merged.
		if vars["ocp4_workload_custom_var"] != "value1" {
			t.Errorf("ocp4_workload_custom_var = %v, want value1", vars["ocp4_workload_custom_var"])
		}
	})

	t.Run("IBMResourceGroupSandbox with creds=true", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"kind": "IBMResourceGroupSandbox",
					"credentials": []interface{}{
						map[string]interface{}{
							"apikey": "ibm-key-123",
							"name":   "rg-name",
						},
					},
					"additional_vars": map[string]interface{}{
						"deployer": map[string]interface{}{
							"ibm_custom_var": "value2",
						},
					},
				},
			},
		}

		vars := extractSandboxVars(placement, true)

		if vars["ibmcloud_api_key"] != "ibm-key-123" {
			t.Errorf("ibmcloud_api_key = %v, want ibm-key-123", vars["ibmcloud_api_key"])
		}
		if vars["ibmcloud_resource_group_name"] != "rg-name" {
			t.Errorf("ibmcloud_resource_group_name = %v, want rg-name", vars["ibmcloud_resource_group_name"])
		}
		if vars["ibm_custom_var"] != "value2" {
			t.Errorf("ibm_custom_var = %v, want value2", vars["ibm_custom_var"])
		}
	})

	t.Run("RosaSandbox with creds=true", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"kind":             "RosaSandbox",
					"name":             "rosa-sandbox-791a8fb2",
					"aws_account_name": "sandbox3104",
					"sa_client_id":     "e2068e83-c2c9-4879-9960-3195c4cc453a",
					"sa_secret":        "foobarRosaSecret",
				},
			},
		}

		vars := extractSandboxVars(placement, true)

		if vars["rosa_sandbox_name"] != "rosa-sandbox-791a8fb2" {
			t.Errorf("rosa_sandbox_name = %v, want rosa-sandbox-791a8fb2", vars["rosa_sandbox_name"])
		}
		if vars["rosa_aws_account_name"] != "sandbox3104" {
			t.Errorf("rosa_aws_account_name = %v, want sandbox3104", vars["rosa_aws_account_name"])
		}
		if vars["rosa_sa_client_id"] != "e2068e83-c2c9-4879-9960-3195c4cc453a" {
			t.Errorf("rosa_sa_client_id = %v, want e2068e83-c2c9-4879-9960-3195c4cc453a", vars["rosa_sa_client_id"])
		}
		if vars["rosa_sa_secret"] != "foobarRosaSecret" {
			t.Errorf("rosa_sa_secret = %v, want foobarRosaSecret", vars["rosa_sa_secret"])
		}
		sandboxes, ok := vars["sandboxes"].([]interface{})
		if !ok || len(sandboxes) != 1 {
			t.Errorf("expected sandboxes with 1 element, got %v", vars["sandboxes"])
		}
	})

	t.Run("RosaSandbox with creds=false", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"kind":             "RosaSandbox",
					"name":             "rosa-sandbox-791a8fb2",
					"aws_account_name": "sandbox3104",
					"sa_client_id":     "e2068e83-c2c9-4879-9960-3195c4cc453a",
					"sa_secret":        "foobarRosaSecret",
				},
			},
		}

		vars := extractSandboxVars(placement, false)

		if vars["rosa_sandbox_name"] != "rosa-sandbox-791a8fb2" {
			t.Errorf("rosa_sandbox_name = %v, want rosa-sandbox-791a8fb2", vars["rosa_sandbox_name"])
		}
		if vars["rosa_aws_account_name"] != "sandbox3104" {
			t.Errorf("rosa_aws_account_name = %v, want sandbox3104", vars["rosa_aws_account_name"])
		}
		if _, ok := vars["rosa_sa_client_id"]; ok {
			t.Error("rosa_sa_client_id should not be present with creds=false")
		}
		if _, ok := vars["rosa_sa_secret"]; ok {
			t.Error("rosa_sa_secret should not be present with creds=false")
		}
		if _, ok := vars["sandboxes"]; ok {
			t.Error("sandboxes should not be present with creds=false")
		}
	})

	t.Run("multi-resource AwsSandbox + RosaSandbox with creds=true", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"kind":           "AwsSandbox",
					"name":           "sandbox3104",
					"account_id":     "202901899156",
					"zone":           "sandbox3104.opentlc.com",
					"hosted_zone_id": "Z05256543CVIM6UZOSDUP",
					"credentials": []interface{}{
						map[string]interface{}{
							"kind":                  "aws_iam_key",
							"name":                  "admin-key",
							"aws_access_key_id":     "foobarKey",
							"aws_secret_access_key": "foobarSecret",
						},
					},
				},
				map[string]interface{}{
					"kind":             "RosaSandbox",
					"name":             "rosa-sandbox-791a8fb2",
					"aws_account_name": "sandbox3104",
					"sa_client_id":     "e2068e83-c2c9-4879-9960-3195c4cc453a",
					"sa_secret":        "foobarRosaSecret",
				},
			},
		}

		vars := extractSandboxVars(placement, true)

		// AWS vars
		if vars["sandbox_name"] != "sandbox3104" {
			t.Errorf("sandbox_name = %v, want sandbox3104", vars["sandbox_name"])
		}
		if vars["aws_access_key_id"] != "foobarKey" {
			t.Errorf("aws_access_key_id = %v, want foobarKey", vars["aws_access_key_id"])
		}
		if vars["sandbox_hosted_zone_id"] != "Z05256543CVIM6UZOSDUP" {
			t.Errorf("sandbox_hosted_zone_id = %v, want Z05256543CVIM6UZOSDUP", vars["sandbox_hosted_zone_id"])
		}

		// ROSA vars
		if vars["rosa_sandbox_name"] != "rosa-sandbox-791a8fb2" {
			t.Errorf("rosa_sandbox_name = %v, want rosa-sandbox-791a8fb2", vars["rosa_sandbox_name"])
		}
		if vars["rosa_aws_account_name"] != "sandbox3104" {
			t.Errorf("rosa_aws_account_name = %v, want sandbox3104", vars["rosa_aws_account_name"])
		}
		if vars["rosa_sa_client_id"] != "e2068e83-c2c9-4879-9960-3195c4cc453a" {
			t.Errorf("rosa_sa_client_id = %v, want e2068e83-c2c9-4879-9960-3195c4cc453a", vars["rosa_sa_client_id"])
		}
		if vars["rosa_sa_secret"] != "foobarRosaSecret" {
			t.Errorf("rosa_sa_secret = %v, want foobarRosaSecret", vars["rosa_sa_secret"])
		}

		// sandboxes deep copy includes both resources
		sandboxes, ok := vars["sandboxes"].([]interface{})
		if !ok || len(sandboxes) != 2 {
			t.Errorf("expected sandboxes with 2 elements, got %v", vars["sandboxes"])
		}
	})

	t.Run("multi-resource AwsSandbox + RosaSandbox with creds=false", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"kind":           "AwsSandbox",
					"name":           "sandbox3104",
					"account_id":     "202901899156",
					"zone":           "sandbox3104.opentlc.com",
					"hosted_zone_id": "Z05256543CVIM6UZOSDUP",
					"credentials": []interface{}{
						map[string]interface{}{
							"kind":                  "aws_iam_key",
							"name":                  "admin-key",
							"aws_access_key_id":     "foobarKey",
							"aws_secret_access_key": "foobarSecret",
						},
					},
				},
				map[string]interface{}{
					"kind":             "RosaSandbox",
					"name":             "rosa-sandbox-791a8fb2",
					"aws_account_name": "sandbox3104",
					"sa_client_id":     "e2068e83-c2c9-4879-9960-3195c4cc453a",
					"sa_secret":        "foobarRosaSecret",
				},
			},
		}

		vars := extractSandboxVars(placement, false)

		// AWS non-cred vars present
		if vars["sandbox_name"] != "sandbox3104" {
			t.Errorf("sandbox_name = %v, want sandbox3104", vars["sandbox_name"])
		}
		if vars["sandbox_zone"] != "sandbox3104.opentlc.com" {
			t.Errorf("sandbox_zone = %v, want sandbox3104.opentlc.com", vars["sandbox_zone"])
		}

		// AWS cred vars absent
		if _, ok := vars["aws_access_key_id"]; ok {
			t.Error("aws_access_key_id should not be present with creds=false")
		}

		// ROSA non-cred vars present
		if vars["rosa_sandbox_name"] != "rosa-sandbox-791a8fb2" {
			t.Errorf("rosa_sandbox_name = %v, want rosa-sandbox-791a8fb2", vars["rosa_sandbox_name"])
		}
		if vars["rosa_aws_account_name"] != "sandbox3104" {
			t.Errorf("rosa_aws_account_name = %v, want sandbox3104", vars["rosa_aws_account_name"])
		}

		// ROSA cred vars absent
		if _, ok := vars["rosa_sa_client_id"]; ok {
			t.Error("rosa_sa_client_id should not be present with creds=false")
		}
		if _, ok := vars["rosa_sa_secret"]; ok {
			t.Error("rosa_sa_secret should not be present with creds=false")
		}

		// No sandboxes deep copy without creds
		if _, ok := vars["sandboxes"]; ok {
			t.Error("sandboxes should not be present with creds=false")
		}
	})

	t.Run("generic kind with creds=true - raw credentials", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"kind": "AzureSandbox",
					"name": "azure-sb",
					"credentials": []interface{}{
						map[string]interface{}{
							"client_id": "cid",
						},
					},
				},
			},
		}

		vars := extractSandboxVars(placement, true)

		creds, ok := vars["credentials"].([]interface{})
		if !ok || len(creds) != 1 {
			t.Errorf("expected credentials with 1 element, got %v", vars["credentials"])
		}
	})

	t.Run("generic kind with creds=false - no credentials", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"kind": "AzureSandbox",
					"name": "azure-sb",
					"credentials": []interface{}{
						map[string]interface{}{
							"client_id": "cid",
						},
					},
				},
			},
		}

		vars := extractSandboxVars(placement, false)

		if _, ok := vars["credentials"]; ok {
			t.Error("credentials should not be present with creds=false")
		}
	})

	t.Run("var annotation routes to named key", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name":           "main-aws",
					"kind":           "AwsSandbox",
					"hosted_zone_id": "Z-MAIN",
					"account_id":     "000000000000",
					"zone":           "main.example.com",
				},
				map[string]interface{}{
					"name":           "extra-aws",
					"kind":           "AwsSandbox",
					"hosted_zone_id": "Z-EXTRA",
					"account_id":     "111111111111",
					"zone":           "extra.example.com",
					"annotations": map[string]interface{}{
						"var": "sandbox2",
					},
				},
			},
		}

		vars := extractSandboxVars(placement, false)

		// Main resource merged at top level.
		if vars["sandbox_name"] != "main-aws" {
			t.Errorf("sandbox_name = %v, want main-aws", vars["sandbox_name"])
		}
		// Annotated resource under its var name.
		sub, ok := vars["sandbox2"].(map[string]interface{})
		if !ok {
			t.Fatalf("sandbox2 not found or wrong type: %v", vars["sandbox2"])
		}
		if sub["sandbox_name"] != "extra-aws" {
			t.Errorf("sandbox2.sandbox_name = %v, want extra-aws", sub["sandbox_name"])
		}
		if sub["sandbox_zone"] != "extra.example.com" {
			t.Errorf("sandbox2.sandbox_zone = %v, want extra.example.com", sub["sandbox_zone"])
		}
	})
}

// --- TestExtractSandboxLabels ---

func TestExtractSandboxLabels(t *testing.T) {
	t.Run("empty resources - empty labels", func(t *testing.T) {
		placement := map[string]interface{}{}
		labels := extractSandboxLabels(placement)
		if len(labels) != 0 {
			t.Errorf("expected empty labels, got %d items", len(labels))
		}
	})

	t.Run("single resource", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name": "my-sandbox",
					"kind": "AwsSandbox",
				},
			},
		}
		labels := extractSandboxLabels(placement)

		if labels["sandbox"] != "my-sandbox" {
			t.Errorf("sandbox = %v, want my-sandbox", labels["sandbox"])
		}
		if labels["AwsSandbox"] != "my-sandbox" {
			t.Errorf("AwsSandbox = %v, want my-sandbox", labels["AwsSandbox"])
		}
	})

	t.Run("multiple resources - different kinds", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name": "aws-sb",
					"kind": "AwsSandbox",
				},
				map[string]interface{}{
					"name": "ocp-sb",
					"kind": "OcpSandbox",
				},
			},
		}
		labels := extractSandboxLabels(placement)

		if labels["sandbox"] != "aws-sb" {
			t.Errorf("sandbox = %v, want aws-sb", labels["sandbox"])
		}
		if labels["AwsSandbox"] != "aws-sb" {
			t.Errorf("AwsSandbox = %v, want aws-sb", labels["AwsSandbox"])
		}
		if labels["OcpSandbox"] != "ocp-sb" {
			t.Errorf("OcpSandbox = %v, want ocp-sb", labels["OcpSandbox"])
		}
	})

	t.Run("multiple resources - same kind with increment", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name": "aws-1",
					"kind": "AwsSandbox",
				},
				map[string]interface{}{
					"name": "aws-2",
					"kind": "AwsSandbox",
				},
			},
		}
		labels := extractSandboxLabels(placement)

		if labels["sandbox"] != "aws-1" {
			t.Errorf("sandbox = %v, want aws-1", labels["sandbox"])
		}
		if labels["AwsSandbox"] != "aws-1" {
			t.Errorf("AwsSandbox = %v, want aws-1", labels["AwsSandbox"])
		}
		if labels["AwsSandbox2"] != "aws-2" {
			t.Errorf("AwsSandbox2 = %v, want aws-2", labels["AwsSandbox2"])
		}
	})

	t.Run("sanitizeKind removes non-alphanumeric chars", func(t *testing.T) {
		placement := map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{
					"name": "sb",
					"kind": "Aws-Sand_box.V2",
				},
			},
		}
		labels := extractSandboxLabels(placement)

		if labels["AwsSandboxV2"] != "sb" {
			t.Errorf("expected AwsSandboxV2 label, got keys: %v", labels)
		}
	})
}
