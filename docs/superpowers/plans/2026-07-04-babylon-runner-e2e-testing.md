# Babylon Runner Helm Chart: E2E Testing Plan (Standalone Repo)

**Goal:** Validate the babylon-runner Helm chart on the dev cluster (`babylon-anarchy-test` namespace) — from image builds through runner coexistence with the existing `default` runner.

**Cluster:** `~/secrets/ocp-babydev.infra-us-east.kubeconfig`
**Namespace:** `babylon-anarchy-test`
**Repo:** `github.com/rhpds/babylon-runner` (standalone)
**Branch:** `sonarqube-fixes` (or `main` after merge)

## Prerequisites

- All SonarQube remediation tasks complete (Tasks 0–6)
- AnarchyRunner spec fix (commit `616b121`) applied in `anarchy` repo
- Kubeconfig available and cluster accessible
- Anarchy operator rebuilt from `anarchy` repo with `ignore-pod-management` annotation support

## Conventions

All commands assume:

```bash
export KUBECONFIG=~/secrets/ocp-babydev.infra-us-east.kubeconfig
export NS=babylon-anarchy-test
```

**Deploy method:** `helm template` + `oc apply` (matches ArgoCD pattern used in production).

**Original images to restore during cleanup:**

- Operator + API: `quay.io/rhpds/anarchy:v0.25.19`
- ImageStream registry: `image-registry.openshift-image-registry.svc:5000/babylon-anarchy-test`

---

### Task 1: Pre-flight — Validate Cluster Access and Pre-conditions

**Purpose:** Ensure the cluster is reachable and the namespace has all prerequisites before modifying anything.

- [x] **Step 1: Verify kubeconfig and namespace**

```bash
oc get ns $NS
oc -n $NS get pods
```

Expected: namespace `babylon-anarchy-test` exists and is Active. Pods running: `anarchy-*` (operator), `anarchy-api-*`, `anarchy-runner-default-*`.

- [x] **Step 2: Verify CRDs installed**

```bash
oc get crd anarchyrunners.anarchy.gpte.redhat.com -o jsonpath='{.spec.versions[0].name}'
oc get crd anarchygovernors.anarchy.gpte.redhat.com -o jsonpath='{.spec.versions[0].name}'
```

Expected: both return `v1`.

- [x] **Step 3: Verify operator and API are healthy**

```bash
oc -n $NS get deployment anarchy -o jsonpath='{.status.readyReplicas}'
oc -n $NS get deployment anarchy-api -o jsonpath='{.status.readyReplicas}'
```

Expected: both return `1` (or more).

- [x] **Step 4: Verify existing runner is functional**

```bash
oc -n $NS get anarchyrunners
oc -n $NS get pods -l anarchy.gpte.redhat.com/runner=default
```

Expected: AnarchyRunner `default` exists. At least one runner pod is Running.

- [x] **Step 5: Record current state for cleanup**

```bash
oc -n $NS get deployment anarchy -o jsonpath='{.spec.template.spec.containers[0].image}'
oc -n $NS get deployment anarchy-api -o jsonpath='{.spec.template.spec.containers[0].image}'
```

Expected: both show `quay.io/rhpds/anarchy:v0.25.19`. Save this for Task 10 restore.

---

### Task 2: Build — Rebuild Operator Anarchy from Anarchy Repo

**Purpose:** Rebuild the Anarchy operator image from the `anarchy` repo (branch with `ignore-pod-management` annotation support). Update operator and API deployments to use the new image.

> **Note:** This task runs from `~/Projects/anarchy/`, not from this repo.

- [x] **Step 1: Verify the ignore-pod-management log line exists in operator**

```bash
grep -n "ignore.pod.management\|Skipping pod management" ~/Projects/anarchy/operator/anarchyrunner.py
```

Expected: log line present in `manage_pods()`. If missing, add it and commit (see original plan Task 2 Steps 1-2).

- [x] **Step 2: Build the operator image from anarchy repo**

```bash
cd ~/Projects/anarchy
oc -n $NS start-build anarchy --from-dir=. --wait --follow
```

Expected: build completes successfully. The BuildConfig `anarchy` already exists and outputs to `istag anarchy:latest`.

- [x] **Step 3: Verify the new image exists**

```bash
oc -n $NS get istag anarchy:latest -o jsonpath='{.image.dockerImageReference}' && echo ""
```

Expected: returns a valid image reference with a new SHA.

- [x] **Step 4: Update operator deployment to use the new image**

```bash
LOCAL_IMAGE="image-registry.openshift-image-registry.svc:5000/$NS/anarchy:latest"
oc -n $NS set image deployment/anarchy manager="$LOCAL_IMAGE"
oc -n $NS rollout status deployment/anarchy --timeout=120s
```

Expected: rollout completes, operator pod restarts with new image.

- [x] **Step 5: Update API deployment to use the new image**

```bash
oc -n $NS set image deployment/anarchy-api api="$LOCAL_IMAGE"
oc -n $NS rollout status deployment/anarchy-api --timeout=120s
```

Expected: rollout completes, API pod restarts with new image.

- [x] **Step 6: Verify both deployments are healthy with new image**

```bash
oc -n $NS get deployment anarchy -o jsonpath='{.spec.template.spec.containers[0].image}'
echo ""
oc -n $NS get deployment anarchy-api -o jsonpath='{.spec.template.spec.containers[0].image}'
echo ""
oc -n $NS get pods
```

Expected: both deployments show the local ImageStream image. All pods are Running and Ready.

- [x] **Step 7: Verify existing runner still works**

```bash
oc -n $NS get anarchyrunners
oc -n $NS get pods -l anarchy.gpte.redhat.com/runner=default
```

Expected: runner `default` still exists and its pod is Running.

---

### Task 3: Build — Build Babylon Runner Image

**Purpose:** Create the ImageStream and BuildConfig for the babylon-runner, then build the Go binary image.

> **Note:** Back to this repo (`~/Projects/babylon-runner/`).

- [x] **Step 1: Create build resources from template**

```bash
cd ~/Projects/babylon-runner
oc -n $NS process -f build-template.yaml --local | oc -n $NS apply -f -
```

Expected: ImageStream `babylon-runner` and BuildConfig `babylon-runner` created.

- [x] **Step 2: Build the babylon-runner image**

```bash
oc -n $NS start-build babylon-runner --from-dir=. --wait --follow
```

Expected: multi-stage Go build completes successfully.

- [x] **Step 3: Verify the image exists**

```bash
oc -n $NS get istag babylon-runner:latest -o jsonpath='{.image.dockerImageReference}' && echo ""
```

Expected: returns a valid image reference.

---

### Task 4: Deploy — Install Babylon Runner Helm Chart

**Purpose:** Render the Helm chart and apply to the cluster using `oc apply` (ArgoCD pattern).

- [ ] **Step 1: Render and review the manifests**

```bash
helm template babylon-runner helm/ \
  --namespace $NS \
  -f helm-vars-dev.yaml \
  > /tmp/babylon-runner-manifests.yaml

cat /tmp/babylon-runner-manifests.yaml
```

Expected: YAML documents rendered. Review for correctness before applying.

- [x] **Step 2: Validate the runner label on pods**

```bash
grep -A1 'runner:' /tmp/babylon-runner-manifests.yaml
```

Expected: `anarchy.gpte.redhat.com/runner: babylon` (NOT `default`).

- [x] **Step 3: Apply the manifests**

```bash
oc -n $NS apply -f /tmp/babylon-runner-manifests.yaml
```

Expected: all resources created/configured.

- [x] **Step 4: Verify resources are up**

```bash
oc -n $NS get sa,cm,deploy,svc,hpa,servicemonitor,anarchyrunner -l app.kubernetes.io/instance=babylon-runner
```

Expected: all resources listed (HPA and ServiceMonitor may be absent if disabled in `helm-vars-dev.yaml`).

---

### Task 5: Verify — Check Resources Created

**Purpose:** Confirm every resource the chart produces exists and has correct configuration.

- [x] **Step 1: ServiceAccount**

```bash
oc -n $NS get sa babylon-runner
```

Expected: ServiceAccount exists.

- [x] **Step 2: ConfigMap**

```bash
oc -n $NS get cm babylon-runner-env -o yaml
```

Expected: ConfigMap exists with keys `RUNNER_NAME: babylon`, `ANARCHY_URL: http://anarchy.babylon-anarchy-test.svc:5000`, `ANARCHY_DOMAIN`, `POLLING_INTERVAL`, etc.

- [x] **Step 3: Deployment**

```bash
oc -n $NS get deployment babylon-runner -o wide
```

Expected: Deployment exists. Image is the local ImageStream image.

- [x] **Step 4: Service**

```bash
oc -n $NS get svc babylon-runner-metrics
```

Expected: Service exists, port 9093.

- [x] **Step 5: AnarchyRunner CR**

```bash
oc -n $NS get anarchyrunner babylon -o yaml
```

Expected: AnarchyRunner `babylon` exists with annotation `anarchy.gpte.redhat.com/ignore-pod-management: "true"` and `spec.minReplicas`/`spec.maxReplicas` set.

---

### Task 6: Verify — Operator Integration (ignore-pod-management)

**Purpose:** Confirm the operator recognizes the AnarchyRunner CR but does NOT create pods for it.

- [x] **Step 1: Check both runners are visible**

```bash
oc -n $NS get anarchyrunners
```

Expected: two runners listed — `default` and `babylon`.

- [x] **Step 2: Check operator logs for the ignore-pod-management message**

```bash
oc -n $NS logs deployment/anarchy --since=2m | grep -i "ignore-pod-management\|Skipping pod management"
```

Expected: log line like `Skipping pod management for runner babylon (ignore-pod-management annotation set)`.

- [x] **Step 3: Verify operator did NOT create pods for babylon runner**

The only pods with label `runner=babylon` should be owned by the Deployment (ReplicaSet), not by the AnarchyRunner CR.

```bash
oc -n $NS get pods -l anarchy.gpte.redhat.com/runner=babylon -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.ownerReferences[0].kind}{"\n"}{end}'
```

Expected: all pods show `ReplicaSet` as owner (from our Deployment), NOT `AnarchyRunner`.

- [x] **Step 4: Verify default runner is unaffected**

```bash
oc -n $NS get pods -l anarchy.gpte.redhat.com/runner=default
```

Expected: default runner pods still Running, managed by operator as before.

---

### Task 7: Verify — Runner Pod Health and API Connectivity

**Purpose:** Confirm the babylon runner pod starts, passes health checks, and authenticates with the Anarchy API.

- [x] **Step 1: Check pod is Running and Ready**

```bash
oc -n $NS get pods -l app.kubernetes.io/name=babylon-runner
```

Expected: at least 1 pod in `Running` state, `READY 1/1`.

- [x] **Step 2: Check pod logs for successful API connection**

```bash
oc -n $NS logs deployment/babylon-runner --tail=50
```

Expected: logs show the runner starting up, connecting to the API URL, and polling for runs. No errors about authentication (401/403), connection refused, or TLS issues.

- [x] **Step 3: Verify health endpoints respond**

The image is a static Go binary (distroless) — no shell tools available. Use port-forward:

```bash
POD=$(oc -n $NS get pod -l app.kubernetes.io/name=babylon-runner -o jsonpath='{.items[0].metadata.name}')
oc -n $NS port-forward $POD 9093:9093 &
sleep 2
curl -s http://localhost:9093/healthz && echo ""
curl -s http://localhost:9093/readyz && echo ""
kill %1
```

Expected: both return a healthy response (200 OK or similar).

- [x] **Step 4: Verify token auth works**

Check the API logs for the babylon runner's polling requests:

```bash
oc -n $NS logs deployment/anarchy-api --tail=50 | grep -i "babylon"
```

Expected: no authentication errors. The runner's polling requests should be accepted.

---

### Task 8: Verify — Runner Coexistence and Performance (default + babylon)

**Purpose:** The critical functional test. Create AnarchySubjects, verify both runners process runs in parallel, collect performance metrics from the Go runner, and record evidence.

**What we're measuring:**

- **Functional:** both runners pick up and complete runs without conflicts
- **Distribution:** how runs split between `default` (Python) and `babylon` (Go)
- **Performance:** run duration, Tower API latency, polling behavior (Go runner only — the Python runner does not expose Prometheus metrics)
- **Reliability:** zero errors across all run types

**This task involves manual interaction:** the user creates AnarchySubjects through the Anarchy interface/UI while we observe the results.

#### Phase 1: Record Baseline

- [ ] **Step 1: Record baseline run count before creating new subjects**

```bash
echo "=== Baseline run count per runner ==="
oc -n $NS get anarchyruns -o jsonpath='{range .items[*]}{.status.runner.name}{"\n"}{end}' | sort | uniq -c | sort -rn

echo ""
echo "=== Total AnarchyRuns ==="
oc -n $NS get anarchyruns --no-headers | wc -l
```

Save these numbers — you will compare against them after creating new subjects.

#### Phase 2: Generate Load

- [ ] **Step 2: Create AnarchySubjects via the UI**

Create multiple AnarchySubjects through the Anarchy interface to generate AnarchyRuns. Create at least 5 subjects to produce enough runs for meaningful distribution.

- [ ] **Step 3: Monitor runs in real time (optional, three terminals)**

Terminal 1 — watch runs being created:

```bash
oc -n $NS get anarchyruns -w
```

Terminal 2 — babylon runner (Go) logs:

```bash
oc -n $NS logs -f deployment/babylon-runner 2>&1 | grep -i "run\|processing\|complete"
```

Terminal 3 — default runner (Python) logs:

```bash
oc -n $NS logs -f -l anarchy.gpte.redhat.com/runner=default 2>&1 | grep -i "run\|processing\|complete"
```

Wait until all subjects finish provisioning (runs reach `successful` status).

#### Phase 3: Collect Results

- [ ] **Step 4: Verify all runs completed successfully (zero failures)**

```bash
echo "=== Run status summary ==="
oc -n $NS get anarchyruns -o jsonpath='{range .items[*]}{.metadata.labels.anarchy\.gpte\.redhat\.com/runner}{"\n"}{end}' | sort | uniq -c | sort -rn

echo ""
echo "=== Last 20 runs (time | name | runner) ==="
oc -n $NS get anarchyruns --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{range .items[*]}{.metadata.creationTimestamp}{"\t"}{.metadata.name}{"\t"}{.status.runner.name}{"\n"}{end}' | tail -20
```

**Pass criteria:** all runs show `successful` status. Both `default` and `babylon` appear in the runner column.

- [ ] **Step 5: Collect distribution numbers**

```bash
echo "=== Total runs per runner ==="
oc -n $NS get anarchyruns -o jsonpath='{range .items[*]}{.status.runner.name}{"\n"}{end}' | sort | uniq -c | sort -rn
```

Record the counts. The Go runner typically absorbs more runs because it polls faster, but both runners must have processed runs.

- [ ] **Step 6: Collect performance metrics from the babylon (Go) runner**

The Go runner exposes Prometheus metrics on port 9093. The Python runner does not expose metrics — this section covers Go runner only.

```bash
POD=$(oc -n $NS get pod -l app.kubernetes.io/name=babylon-runner -o jsonpath='{.items[0].metadata.name}')
oc -n $NS port-forward $POD 9093:9093 &
sleep 2

echo "=== Runs processed (by handler/action/status) ==="
curl -s http://localhost:9093/metrics | grep ^babylon_runner_runs_total

echo ""
echo "=== Run duration (histogram) ==="
curl -s http://localhost:9093/metrics | grep ^babylon_runner_run_duration_seconds

echo ""
echo "=== Tower API latency ==="
curl -s http://localhost:9093/metrics | grep ^babylon_runner_tower_job_duration

echo ""
echo "=== Polling behavior ==="
curl -s http://localhost:9093/metrics | grep ^babylon_runner_poll_duration

echo ""
echo "=== Current state ==="
curl -s http://localhost:9093/metrics | grep ^babylon_runner_active_run

kill %1 2>/dev/null
```

**Available metrics and what they mean:**

| Metric                                          | Type      | What it measures                                                 |
| ----------------------------------------------- | --------- | ---------------------------------------------------------------- |
| `babylon_runner_runs_total`                     | Counter   | Total runs by `handler_type`, `action`, `status` (success/error) |
| `babylon_runner_run_duration_seconds`           | Histogram | End-to-end time per run, by handler and action                   |
| `babylon_runner_tower_job_duration_seconds`     | Histogram | Latency of each HTTP call to Tower/AAP                           |
| `babylon_runner_sandbox_api_duration_seconds`   | Histogram | Latency of Sandbox API calls                                     |
| `babylon_runner_scheduler_api_duration_seconds` | Histogram | Latency of Scheduler API calls                                   |
| `babylon_runner_poll_duration_seconds`          | Histogram | Time spent on each GET /run poll (includes long-poll waits)      |
| `babylon_runner_active_run`                     | Gauge     | 1 if processing a run, 0 if idle                                 |

**How to read histogram output:**

- `_count` = total number of observations
- `_sum` = total seconds across all observations (divide by `_count` for average)
- `_bucket{le="X"}` = how many observations completed in ≤ X seconds

Example: if `_sum{action="provision"}` = 58.36 and `_count{action="provision"}` = 32, then average = 58.36 / 32 = **1.82s per provision**.

#### Phase 4: Record Evidence

- [ ] **Step 7: Summarize results**

Fill in this template with the collected data:

```markdown
## E2E Test Results — Runner Coexistence

Date: YYYY-MM-DD
Cluster: ocp-babydev / babylon-anarchy-test
Babylon Runner Image: <image SHA from Task 3>

### Distribution

- Total runs: \_\_\_
- babylon (Go): **_ (_**%)
- default (Python): **_ (_**%)
- Failures: \_\_\_

### Performance (Go runner only)

- Provisioning: *** runs, avg ***s
- Tower API: *** calls, avg ***ms, \_\_\_% < 500ms
- Subject create: *** runs, avg ***ms
- Active run at collection time: idle / processing

### Verdict

PASS / FAIL — reason: \_\_\_
```

**Pass criteria:**

1. Both runners processed at least 1 run
2. Zero failures across all run types
3. No Tower API calls > 5s
4. No provisioning runs > 30s

---

### Task 10: Cleanup — Restore Cluster to Original State

**Purpose:** Remove all test artifacts and restore the namespace to its pre-test state.

- [ ] **Step 1: Remove the babylon-runner manifests**

```bash
oc -n $NS delete -f /tmp/babylon-runner-manifests.yaml
```

Expected: all chart resources removed.

- [ ] **Step 2: Verify all chart resources are removed**

```bash
oc -n $NS get sa,cm,deploy,svc,hpa,servicemonitor,anarchyrunner -l app.kubernetes.io/instance=babylon-runner
```

Expected: no resources found.

- [ ] **Step 3: Remove babylon-runner build resources**

```bash
oc -n $NS delete bc babylon-runner
oc -n $NS delete is babylon-runner
```

Expected: BuildConfig and ImageStream removed.

- [ ] **Step 4: Restore operator and API to original image**

```bash
ORIGINAL_IMAGE="quay.io/rhpds/anarchy:v0.25.19"
oc -n $NS set image deployment/anarchy manager="$ORIGINAL_IMAGE"
oc -n $NS set image deployment/anarchy-api api="$ORIGINAL_IMAGE"
oc -n $NS rollout status deployment/anarchy --timeout=120s
oc -n $NS rollout status deployment/anarchy-api --timeout=120s
```

Expected: both deployments rollback to original image, pods restart and become Ready.

- [ ] **Step 5: Verify cluster is restored**

```bash
oc -n $NS get pods
oc -n $NS get anarchyrunners
oc -n $NS get deployment anarchy -o jsonpath='{.spec.template.spec.containers[0].image}'
echo ""
oc -n $NS get deployment anarchy-api -o jsonpath='{.spec.template.spec.containers[0].image}'
```

Expected: only the original pods (`anarchy-*`, `anarchy-api-*`, `anarchy-runner-default-*`), one AnarchyRunner (`default`), both deployments using `quay.io/rhpds/anarchy:v0.25.19`.
