# TLS Consistency & PQC Readiness for OCP 4.22 – NRO Change Analysis

This document summarizes the **OCP 4.22 TLS/PQC initiative** requirements and the **changes needed** in the numaresources-operator (NRO) repository. It aligns with the official **TLS Profile Compliance Remediation Guidance** and FAQ (signoff: Joe Lanford, Mrunal Patel, JP Jung, Lance Bragstad, Nicholas Richardson, James Stallings, Shaun Smith).

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

**Preferred approach:** Use the shared package **[openshift/controller-runtime-common/pkg/tls](https://github.com/openshift/controller-runtime-common/tree/main/pkg/tls)**. It provides `FetchAPIServerTLSProfile`, `NewTLSConfigFromProfile` (for controller-runtime TLSOpts), and `SecurityProfileWatcher` (to trigger graceful restart when the cluster TLS profile changes). See [cluster-machine-approver PR #286](https://github.com/openshift/cluster-machine-approver/pull/286) for the exact pattern (client before manager, TLSOpts for webhook/metrics, watcher with `OnProfileChange: cancel`). The steps below align with what that package does; do not reimplement profile resolution or cipher mapping in NRO.

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

**Upstream-friendly path (recommended):** RTE accepts an **optional generic** TLS profile (min version + cipher list, and later curves) via flags or config—no OpenShift API in RTE. When deployed on OpenShift, NRO reads the API Server profile, converts to that format, and injects it (e.g. container args or ConfigMap). RTE remains deployable on vanilla Kubernetes; compliance is achieved on OpenShift via NRO.

**Upstream-friendly recommendation:** RTE adds optional, platform-agnostic TLS profile input (flags/config); NRO injects the cluster profile when deploying on OpenShift. See **[TLS_PQC_RTE_UPSTREAM_FRIENDLY.md](./TLS_PQC_RTE_UPSTREAM_FRIENDLY.md)** for the full contract and division of responsibility (RTE: generic profile in; NRO: API Server profile → RTE format).

### 3.3 CSV and feature flag

- In **bundle/manifests/numaresources-operator.clusterserviceversion.yaml** (and any other CSVs that declare the feature):
  - Change `features.operators.openshift.io/tls-profiles: "false"` to **`"true"`** once the operator (and, if applicable, RTE) honor the cluster TLS profile.

### 3.4 Recommended: library-go (official guidance)

Per the **TLS Profile Compliance Remediation Guidance**:

- **Most OpenShift operators** should use the **library-go configobserver pattern** (recommended approach). This is the standard pattern across the platform.
- **library-go** provides:
  - **`ObserveTLSSecurityProfile`** (apiserver config observer): observes API Server TLS profile via `APIServerLister().Get("cluster")`, converts OpenSSL cipher names to IANA (for ServingInfo), sets `servingInfo.minTLSVersion` and `servingInfo.cipherSuites`. See `library-go/pkg/operator/configobserver/apiserver`.
  - **Curve preferences** will be added once **openshift/api#2583** is merged and library-go is updated.
- **NRO today**: Does **not** use library-go; it uses controller-runtime with **direct `crypto/tls.Config`** (webhook + metrics servers). So the guidance path is:
  - **Option 1 (recommended):** Add **library-go** and use the apiserver config observer; then convert the observed config (minTLSVersion, cipherSuites, and curves when available) into `*tls.Config` for webhook/metrics via library-go crypto utilities where applicable.
  - **Option 2:** If not using the full configobserver pattern, use **direct Go application code** approach: fetch `TLSSecurityProfile` from API Server, extract profile spec (built-in vs custom), convert **OpenSSL-style cipher names** (and curve names when available) to **Go `crypto/tls` constants**, and set all TLS config explicitly—do not rely on Go defaults. Note: OpenShift profiles use OpenSSL-style names (e.g. `ECDHE-RSA-AES128-GCM-SHA256`); Go requires numeric/constant IDs (e.g. `tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256`).

**Hardcoding "TLS 1.3" is not acceptable;** the component must remain compliant as the central policy is updated without code change.

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

## 6. Official remediation steps (alignment)

The guidance defines three steps; NRO mapping:

| Step | Guidance | NRO action |
|------|----------|------------|
| **1. Identify the source** | Find where TLS is set: app code, deps, config, env, underlying components (haproxy, openssl, etc.). | **Done:** TLS is in app code—webhook and metrics use controller-runtime TLS; RTE uses its own TLS in the binary. Ensure profile is passed to **all** layers (no underlying proxy without profile). |
| **2. Update the configuration** | Remove hardcoding; fetch from API Server (default). Operators: library-go configobserver; direct Go: fetch profile, convert OpenSSL→Go constants, set all settings explicitly. | Implement Section 3 (operator webhook/metrics + RTE). Use library-go or direct fetch + conversion; add curves when openshift/api#2583 is merged. |
| **3. Verify compliance** | Network: **tls-scanner**; code: **semgrep** rules; functional: start, accept permitted, reject non-compliant, respond to profile changes. | Run tls-scanner on operator and RTE endpoints; run HPCASE semgrep rules; add functional tests. |

---

## 7. Next steps (actionable)

Follow this order, aligned with the FAQ timeline and remediation guidance.

### Phase 1: OCP 4.22 – mandatory (release blocker)

1. **ML-KEM (mandatory)**  
   - **Test:** "Does the TLS server negotiate TLS 1.3 with ML-KEM if the client supports it?" (even with Intermediate profile).  
   - **Report:** Add/link your component Jira to **OCPSTRAT-2361** (PQC Testing - OCP Core & Platform Aligned Operators). Must pass for 4.22 GA.

2. **TLS profile for operator (webhook + metrics)**  
   - Implement Section 3.1: fetch API Server TLS profile (default source), convert to `*tls.Config` (MinVersion, CipherSuites; no reliance on Go defaults), pass via TLSOpts to webhook and metrics.  
   - Prefer **library-go** configobserver + crypto if you add the dependency; otherwise direct fetch + OpenSSL→Go cipher/curve conversion.  
   - Add RBAC for `apiservers.config.openshift.io` (get/list/watch as needed).

3. **Verification**  
   - **tls-scanner:** Run against operator metrics and webhook endpoints; confirm only permitted profile is accepted.  
   - **Semgrep:** Run HPCASE Argus Observe rules; fix any real violations (ignore false positives).  
   - **Functional:** Component starts, accepts permitted clients, rejects non-compliant, and (if you support dynamic updates) responds to profile changes.

### Phase 2: OCP 4.22 – Tech Preview (once APIs merge)

4. **TLSAdherence (Tech Preview)**  
   - Wait for API merge for tlsAdherence (track HPCASE-180 / OCPSTRAT-2916).  
   - Implement and test with **tlsAdherence: Strict**.

5. **TLSCurvePreferences (Tech Preview)**  
   - Wait for **openshift/api#2583** (merge support for TLS curves in OpenShift API) and library-go updates.  
   - Add **curve preferences** to your TLS helper and apply to webhook, metrics (and RTE if applicable).  
   - Verify curves are respected (tls-scanner / functional tests).

6. **Operands (RTE)**  
   - Align with RTE owners: either RTE accepts profile from NRO (ConfigMap/env/args) or RTE reads cluster profile itself.  
   - Ensure RTE DaemonSet is reconciled when API Server TLS config changes if NRO passes profile.  
   - Run tls-scanner on RTE metrics endpoint.

7. **Scheduler (deployer)**  
   - Ensure the scheduler Deployment (when deployed by NRO) receives cluster TLS profile (e.g. `--tls-min-version`, `--tls-cipher-suites`) from API Server; see [TLS_PQC_4.22_SCHEDULER_PLUGINS_ANALYSIS.md](./TLS_PQC_4.22_SCHEDULER_PLUGINS_ANALYSIS.md).

8. **CSV**  
   - Set `features.operators.openshift.io/tls-profiles: "true"` when operator (and operands) honor cluster TLS profile.

### Phase 3: OCP 5.0 – GA

9. **TLSAdherence & TLSCurvePreferences GA**  
   - Both feature gates GA in 5.0; components must honor cluster TLS profile with tlsAdherence: Strict.  
   - Missing components get Critical bugs, backported to 5.0.

### If you miss 4.22 GA for Tech Preview

- Use **OCP Exception Jira process**: clone **OCPEXCEPT-50**, keep `[TLS_1.3-PQC]` header, specify feature gate (TLSAdherence / TLSCurvePreferences), reason, and **remediation time** (4.22 zStream acceptable; 5.0 is not). If approved, complete and move to Done.

### Contacts and resources

- **Forum:** **#forum-ocp-tls-strict-obedience** (questions, updates).  
- **Contacts:** JP Jung, Mrunal Patel, Lance Bragstad, Joe Lanford, Shaun Smith, Nicholas Richardson.  
- **Tools:** HPCASE **tls-scanner** (network verification); **Argus Observe / semgrep** rules (code-level).  
- **Docs:** [TLS Security Profiles - OCP](https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html/security_and_compliance/tls-security-profiles); Code Examples tab in the remediation guidance.

---

## 8. References (from your context)

- **OCPSTRAT-2611** – Centralized & enforced TLS configuration (Core & layered).
- **OCPSTRAT-2361** – PQC testing (report ML-KEM results here).
- **OCPSTRAT-2916** – [APIserver] API changes for TLS consistency.
- **HPCASE-180** – tlsAdherence API (Tech Preview); **HPCASE-88** – CASE epic.
- **openshift/api#2583** – Merge support for TLS curves in OpenShift API.
- **library-go** – `pkg/operator/configobserver/apiserver` (ObserveTLSSecurityProfile); crypto helpers for TLS.
- **TLS Profile Compliance Remediation Guidance** – resolution steps, code examples, tools (tls-scanner, semgrep).
- **FAQ and technical implementation guidelines** – for resolution steps, code samples, and API details.
- **#forum-ocp-tls-strict-obedience** – for questions and updates.

This gives you a clear map of what “no hardcoded TLS” and “use cluster TLS profile” mean for NRO and what to change in this repo.
