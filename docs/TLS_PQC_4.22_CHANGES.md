# TLS Consistency & PQC Readiness for OCP 4.22 – NRO Change Analysis

This document summarizes the **OCP 4.22 TLS/PQC initiative** requirements and the **changes needed** in the numaresources-operator (NRO) repository.

---

## 1. Initiative summary (what’s required)

- **Goal**: Components must **not** hardcode TLS; they must **obtain TLS configuration from the cluster** (default: **API Server** configuration: `apiservers.config.openshift.io/cluster`).
- **Scope**: Applies to **all TLS servers** in the component:
  - The **operator process** (webhook server, metrics server).
  - **Operands** (workloads the operator deploys that run TLS servers), e.g. RTE metrics over TLS.
- **Server-side only**: TLS **client** settings are out of scope.
- **OCP 4.22** (Tech Preview): TLSAdherence and TLSCurvePreferences feature gates; implement and test with `tlsAdherence: Strict` once API is merged.
- **ML-KEM**: TLS servers must support and offer **ML-KEM** when the client supports it (TLS 1.3 with hybrid ML-KEM). Go 1.24+ and OCP 4.22 provide this; the main requirement is to **use the cluster TLS profile** (so when the cluster moves to Modern/TLS 1.3, NRO follows).

Acceptance criteria that matter for this repo:

- Remove any **local/hardcoded** TLS settings (protocols, ciphers, curves).
- **Fetch and apply** the TLS policy from the central source (API Server by default).
- **Do not rely on Go defaults** for TLS; explicitly set MinVersion, CipherSuites (and curves when the API is available).
- Component is **PQC-ready** by honoring the configured TLS profile.

---

## 2. Current state in this repo

### 2.1 Operator’s own TLS servers

| Component        | Location              | Current behavior |
|-----------------|-----------------------|------------------|
| **Webhook server** | `cmd/main.go`         | Uses `webhook.NewServer()` with `TLSOpts: webhookTLSOpts(params.enableHTTP2)`. Only **NextProtos** (HTTP/1.1 vs HTTP/2) are set. **No** MinVersion, CipherSuites, or CurvePreferences → **relies on Go defaults**. |
| **Metrics server** | `cmd/main.go`         | Uses `metricsserver.Options{BindAddress, SecureServing: true, CertDir: "/certs"}`. **No TLSOpts** → **relies on Go defaults**. |

So today the operator **does not** read the cluster TLS profile and does **not** explicitly set TLS version/ciphers/curves for either server.

### 2.2 Operands that use TLS

| Operand | TLS usage | Where configured |
|--------|-----------|-------------------|
| **RTE (Resource Topology Exporter)** | Metrics server with `--metrics-mode=httptls` (TLS on port 2112). Certs from secret `rte-metrics-service-cert`. | `pkg/objectupdate/rte/rte.go` (e.g. `DaemonSetArgs` sets `--metrics-mode=httptls`; volume for `rte-metrics-service-cert`). RTE binary (from `resource-topology-exporter`) builds its own `tls.Config`; it does **not** receive cluster TLS profile from NRO. |
| **Scheduler plugin** | No TLS server identified in this repo; it’s a scheduler, not an HTTPS endpoint. | N/A for server TLS. |

So the only operand TLS server in scope is **RTE’s metrics HTTPS server**. Today it uses whatever TLS config the RTE binary uses (likely Go defaults), not the cluster profile.

### 2.3 Other relevant bits

- **CSV**: `features.operators.openshift.io/tls-profiles: "false"` → must become **"true"** once NRO supports cluster TLS profiles.
- **Dependencies**: Repo has `openshift/api` and `openshift/client-go` (including config client) in vendor. It does **not** use `library-go` (e.g. no apiserver config observer or `library-go/pkg/crypto`).
- **API**: `configv1.TLSSecurityProfile` and `configv1.TLSProfiles` (Old/Intermediate/Modern) are in `vendor/github.com/openshift/api/config/v1/`. APIServer spec has `TLSSecurityProfile`; canonical resource is `apiservers.config.openshift.io` named **cluster**.

---

## 3. Changes required

### 3.1 Operator process: webhook and metrics servers

**Objective:** Both servers must use TLS settings derived from **API Server** `tlsSecurityProfile` (cluster profile), not Go defaults.

1. **Read cluster TLS profile at startup**
   - Before creating the controller manager, get the API Server config:
     - Resource: `config.openshift.io/v1`, `APIServer`, name **`cluster`**.
   - Use existing `openshift/client-go` config client (e.g. `configclient.NewForConfig(restConfig)` → `APIServers().Get(ctx, "cluster", metav1.GetOptions{})`).
   - If the resource does not exist (e.g. non-OpenShift or cluster not ready), define a **fallback** (e.g. Intermediate profile or a safe default consistent with the FAQ: “new clusters deploy with intermediate profile, i.e. TLS 1.2”).
   - Compute the **effective** `TLSProfileSpec` from `Spec.TLSSecurityProfile`:
     - Nil or unset → default to **Intermediate** (as per FAQ).
     - Type Old/Intermediate/Modern → use `configv1.TLSProfiles[type]`.
     - Type Custom → use `Custom.TLSProfileSpec` (Ciphers + MinTLSVersion).

2. **Convert profile to `crypto/tls.Config`**
   - Implement (or reuse) a helper that maps:
     - `configv1.TLSProfileSpec` → `MinVersion` and `CipherSuites` (and later `CurvePreferences` when TLSCurvePreferences is in place).
   - Map `configv1.TLSProtocolVersion` to `tls.VersionTLS10/11/12/13`.
   - Map cipher names (e.g. `TLS_AES_128_GCM_SHA256`) to `tls.CipherSuite` IDs; filter to cipher suites supported by the Go version (e.g. `tls.CipherSuites()`).
   - **Do not** hardcode a single profile (e.g. “TLS 1.3 only”); the config must come from the cluster and support Old/Intermediate/Modern/Custom.

3. **Apply TLS config to webhook and metrics**
   - **Webhook**: Keep existing `TLSOpts` (e.g. HTTP/2 toggle). **Append** TLSOpts that set `MinVersion`, `CipherSuites` (and when available `CurvePreferences`) from the cluster profile. Same effective `*tls.Config` as used below.
   - **Metrics**: Today `metricsserver.Options` has no `TLSOpts`. **Add** `TLSOpts` so the metrics server uses the **same** cluster-derived TLS config (MinVersion, CipherSuites, curves when applicable).

4. **RBAC**
   - Ensure the operator has permission to **get** (and optionally **list/watch**) `apiservers.config.openshift.io` (resource `apiservers`, group `config.openshift.io`). Today the controller has RBAC for `clusteroperators`, `clusterversions`, `infrastructures`; add **apiservers** as needed.

5. **Non-OpenShift / bootstrap**
   - When running outside OpenShift or when `apiservers.config.openshift.io` is not available, do not fail hard: use a defined fallback (e.g. Intermediate) so the operator can still start and pass tests.

### 3.2 Operands: RTE

**Objective:** “TLS settings should apply to all CRs that an operator manages” — so RTE’s TLS metrics server should honor the cluster TLS profile.

- **Option A – RTE supports profile injection**
  - If the RTE binary (or its options) can accept TLS profile parameters (e.g. min version, cipher list or profile name), NRO should:
    - Pass the **same** effective TLS profile (or its serialized form) to RTE (e.g. via ConfigMap, env, or args).
    - Ensure the RTE DaemonSet is updated when the cluster TLS profile changes (e.g. reconcile when APIServer `cluster` changes).
  - This may require changes in the **resource-topology-exporter** repo (e.g. new flags or config) and then consumption in NRO (e.g. in `pkg/objectupdate/rte` and the NRO controller that reconciles RTE).
- **Option B – RTE uses cluster profile by default**
  - If RTE is updated upstream to read the API Server TLS profile itself (e.g. via a sidecar or library), then NRO only needs to ensure it deploys a RTE version that does that; no NRO code change for passing profile.
- **Option C – Document and track**
  - If RTE cannot yet honor the cluster profile, document the gap and track (e.g. Jira) until RTE supports it; NRO would still be required to fix **operator** TLS (webhook + metrics) as above.

Recommendation: Align with the RTE owners and OCP TLS/PQC guidance; implement Option A if RTE can accept profile parameters, so NRO explicitly passes the cluster profile to the operand.

### 3.3 CSV and feature flag

- In **bundle/manifests/numaresources-operator.clusterserviceversion.yaml** (and any other CSVs that declare the feature):
  - Change `features.operators.openshift.io/tls-profiles: "false"` to **`"true"`** once the operator (and, if applicable, RTE) honor the cluster TLS profile.

### 3.4 Optional: library-go

- Many OpenShift operators use **library-go** for:
  - **Config observer** that watches `apiservers.config.openshift.io/cluster` and exposes the TLS profile.
  - **Crypto helpers** that turn `configv1.TLSSecurityProfile` into `*tls.Config` (including cipher name → ID mapping and curve handling).
- This repo does **not** currently depend on library-go. You can:
  - **Option 1:** Add `library-go` and use its observer + crypto helpers (reduces custom code and stays aligned with other OCP operators).
  - **Option 2:** Keep no dependency on library-go and implement:
    - One-time (or cached) read of APIServer `cluster` and conversion from `TLSSecurityProfile` to `*tls.Config` in this repo.

Both are acceptable as long as the operator **does not** hardcode TLS and **does** apply the cluster profile to webhook and metrics (and to RTE if Option A is chosen).

### 3.5 OCP 4.22 Tech Preview (TLSAdherence / TLSCurvePreferences)

- **TLSAdherence**: Once the API for `tlsAdherence` is merged (see HPCASE-180), implement and test with `tlsAdherence: Strict` as required for your component.
- **TLSCurvePreferences**: When the curves API is merged, apply **curve preferences** from the cluster profile to the same `*tls.Config` used for webhook and metrics (and to RTE if applicable).
- These may require additional fields in the API and in your TLS helper (e.g. `CurvePreferences` on `tls.Config`); the exact types can be taken from the merged API and the technical guide.

### 3.6 ML-KEM and testing

- **Code**: Using the cluster TLS profile (and TLS 1.3 when the profile is Modern or allows it) with a recent Go build (e.g. Go 1.24+) gives ML-KEM support where the client supports it; no extra code in NRO beyond applying the profile.
- **Testing**: Report ML-KEM test results to **OCPSTRAT-2361** and link any component Jira to it. Run **tls-scanner** (or the recommended scanner) against the operator’s metrics and webhook endpoints (and RTE metrics if applicable) to confirm compliance with the cluster TLS policy.

---

## 4. File-level checklist

| Area | Action |
|------|--------|
| **cmd/main.go** | 1) Before building the manager, get APIServer `cluster` and compute effective TLS profile. 2) Convert profile to `*tls.Config` (MinVersion, CipherSuites; later CurvePreferences). 3) Pass TLSOpts that apply this config to both webhook and metrics servers. 4) Keep HTTP/2 behavior in webhook TLSOpts. 5) Handle non-OpenShift / missing APIServer (fallback profile). |
| **New or existing pkg** | Add a small TLS helper: e.g. `pkg/tls/profile.go` (or under `internal/`) to: get effective `TLSProfileSpec` from `*configv1.TLSSecurityProfile`, and convert it to `*tls.Config` (and optionally to a form consumable by RTE). |
| **RBAC** | Add permission to get (and if doing dynamic updates, list/watch) `apiservers` in `config.openshift.io`. |
| **RTE (pkg/objectupdate/rte, controller)** | If Option A: pass cluster TLS profile to RTE (e.g. ConfigMap/env/args) and ensure RTE DaemonSet is reconciled when APIServer TLS config changes. |
| **CSV** | Set `features.operators.openshift.io/tls-profiles: "true"` when implementation is complete. |
| **Tests** | Add/update tests: TLS config derived from APIServer (and fallback), and unit tests for profile → tls.Config. |
| **Docs** | Optionally document that NRO uses the API Server TLS profile and that RTE (if applicable) is configured to use the same profile. |

---

## 5. Order of implementation (suggested)

1. **RBAC**: Add `apiservers.config.openshift.io` get (and list/watch if you do dynamic updates).
2. **TLS helper**: Implement effective-profile resolution and `TLSProfileSpec` → `*tls.Config` (MinVersion + CipherSuites; CurvePreferences when API is ready).
3. **main.go**: Before creating the manager, get APIServer `cluster`, compute profile, build TLSOpts, and pass them to webhook and metrics.
4. **Tests**: Unit tests for profile resolution and conversion; integration/e2e for “metrics and webhook use cluster profile.”
5. **RTE**: Coordinate with RTE; implement passing profile to RTE (Option A) if supported.
6. **CSV**: Set `tls-profiles: "true"`.
7. **TLSAdherence / TLSCurvePreferences**: Integrate when APIs are merged; add curve preferences to the same TLS helper and TLSOpts.
8. **ML-KEM / compliance**: Run tls-scanner and report to OCPSTRAT-2361; link component Jira.

---

## 6. References (from your context)

- **OCPSTRAT-2611** – Centralized & enforced TLS configuration (Core & layered).
- **OCPSTRAT-2361** – PQC testing (report ML-KEM results here).
- **OCPSTRAT-2916** – [APIserver] API changes for TLS consistency.
- **HPCASE-180** – tlsAdherence API (Tech Preview); **HPCASE-88** – CASE epic.
- **FAQ and technical implementation guidelines** – for resolution steps, code samples, and API details.
- **#forum-ocp-tls-strict-obedience** – for questions and updates.

This gives you a clear map of what “no hardcoded TLS” and “use cluster TLS profile” mean for NRO and what to change in this repo.
