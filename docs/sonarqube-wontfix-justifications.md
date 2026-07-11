# SonarQube Won't Fix Justifications

Justifications for findings marked as **Won't Fix** in SonarQube.
These are not false positives — the analyzer is correctly identifying
the pattern — but each one is an intentional, risk-accepted design
decision documented below.

## 1. go:S5527 — Enable server hostname verification on this SSL/TLS connection

**File:** `internal/httputil/transport.go:29`
**Severity:** CRITICAL (VULNERABILITY)

**Justification:** TLS hostname verification is controlled at runtime by the
`TOWER_TLS_VERIFY` environment variable (exposed in Helm as
`config.towerTLSVerify`, defaults to `"true"`). When `TOWER_TLS_VERIFY=false`,
`InsecureSkipVerify` is set intentionally to support environments where
Ansible Automation Platform (Tower) uses self-signed certificates or
internal CAs not in the system trust store. The function also supports
loading a custom CA bundle via `TOWER_CA_CERT` as a secure alternative
to disabling verification entirely. This is a controlled escape hatch,
not a default — production deployments use verified TLS.

## 2. go:S4830 — Enable server certificate validation on this SSL/TLS connection

**File:** `internal/httputil/transport.go:29`
**Severity:** CRITICAL (VULNERABILITY)

**Justification:** Same line and same rationale as go:S5527 above. Both
rules flag the single `InsecureSkipVerify: true` statement. Certificate
validation is only disabled when the operator explicitly sets
`TOWER_TLS_VERIFY=false`. The Helm chart defaults to `towerTLSVerify: "true"`.
A custom CA path (`TOWER_CA_CERT`) is supported as the preferred
alternative to disabling validation.

## 3. kubernetes:S6418 — Make sure this is not a hard-coded secret

**File:** `helm/templates/deployment.yaml:38`
**Severity:** BLOCKER (VULNERABILITY)

**Justification:** The `RUNNER_TOKEN` value is not a hard-coded secret.
It is dynamically generated at template rendering time using SHA-256:

```
$tokenInput := printf "%s-%s-babylon-runner-%s" .Release.Namespace .Release.Name .Values.auth.tokenSalt
$token := $tokenInput | sha256sum | trunc 32
```

The token is a deterministic hash of the release namespace, release name,
and a configurable salt (`auth.tokenSalt`). It changes when any of these
inputs change. SonarQube flags the `value:` field in the env var as a
potential hard-coded secret, but the value is a Helm template expression
evaluated at deploy time, not a static string in source code.

## 4. kubernetes:S5332 — Make sure that using clear-text protocols is safe here

**File:** `helm/templates/configmap-env.yaml:11`
**Severity:** CRITICAL (SECURITY_HOTSPOT)

**Justification:** The `ANARCHY_URL` defaults to
`http://anarchy.<namespace>.svc:5000` — an HTTP (not HTTPS) URL. This is
intentional because the runner communicates with the Anarchy API over the
Kubernetes cluster-internal network using ClusterIP Services. Intra-cluster
`svc` traffic does not leave the cluster network and does not traverse any
untrusted network. This is the standard Kubernetes service-to-service
communication pattern. TLS for intra-cluster traffic would add operational
complexity (certificate management, rotation) with no security benefit in
this context.

## 5. kubernetes:S6596 — Specific version tag for image should be used

**File:** `helm/templates/deployment.yaml:31`
**Severity:** MAJOR (CODE_SMELL)

**Justification:** The image tag is not hard-coded — it is resolved
dynamically by the `babylon-runner.image` helper template in
`_helpers.tpl`. The template selects the tag based on `image.tagOverride`
or `version` values, supporting `:latest`, a specific version tag, or a
bare repository reference. In production, the CI/CD pipeline sets the
exact version via `values.version` (e.g., `v1.2.3`). SonarQube cannot
evaluate Helm template expressions and sees only the Go template syntax,
not the resolved image reference.
