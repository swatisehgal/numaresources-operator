# TLS Profile Consistency – Topology Aware Scheduler Stack Evaluation

*(Same structure as MetalLB TLS evaluation for CNF-21983)*

**Epic:** [CNF-21765](https://issues.redhat.com/browse/CNF-21765)

---

## Goal

TLS curves and suites must be configurable for all components; all TLS endpoints must support Post-Quantum Cryptography (PQC) requirements, including **X25519MLKEM768** (hybrid quantum-resistant key exchange). On OpenShift, the platform-wide TLS security profile is configured via **apiservers.config.openshift.io** (cluster-scoped). When **openshift/api PR #2583** merges, this profile will include cipher suites and elliptic curve preferences (including PQC via X25519MLKEM768).

Components must:
- Remove hardcoded TLS; fetch and apply the TLS policy from the central source (API Server by default).
- Explicitly set MinVersion, CipherSuites, and CurvePreferences (when API is available)—do not rely on Go defaults.
- Be PQC-ready in one pass by honoring the full configured TLS profile.

**Repositories:**
- [openshift-kni/numaresources-operator](https://github.com/openshift-kni/numaresources-operator/)
- [openshift-kni/scheduler-plugins](https://github.com/openshift-kni/scheduler-plugins)
- [k8stopologyawareschedwg/resource-topology-exporter](https://github.com/k8stopologyawareschedwg/resource-topology-exporter/)

---

## Proposed solution for controller-runtime–based operators

For OpenShift operators that use **controller-runtime** for reconciliation and controllers, the recommended approach is to use the shared Go package:

- **Package:** [openshift/controller-runtime-common/pkg/tls](https://github.com/openshift/controller-runtime-common/tree/main/pkg/tls)
- **Example usage:** [openshift/cluster-machine-approver PR #286](https://github.com/openshift/cluster-machine-approver/pull/286) – “tls: use centralized TLS profile”

### What the package provides

| API | Purpose |
|-----|--------|
| `FetchAPIServerTLSProfile(ctx, k8sClient)` | Reads `apiservers.config.openshift.io/cluster`, returns effective `configv1.TLSProfileSpec` (resolves Old/Intermediate/Modern/Custom; default Intermediate if nil). |
| `GetTLSProfileSpec(profile *configv1.TLSSecurityProfile)` | Resolves a TLSSecurityProfile to TLSProfileSpec (used internally by FetchAPIServerTLSProfile). |
| `NewTLSConfigFromProfile(profile configv1.TLSProfileSpec)` | Returns `(func(*tls.Config), unsupportedCiphers []string)` suitable for controller-runtime **TLSOpts**. Sets MinVersion and CipherSuites (CurvePreferences TODO when openshift/api#2583 merges). Uses `openshift/library-go/pkg/crypto` for cipher/version mapping. |
| `SecurityProfileWatcher` | Controller that watches the APIServer resource; when the TLS profile **changes** from the initial one, calls `OnProfileChange(ctx, oldSpec, newSpec)`. Typical use: callback calls `cancel()` on the main context to trigger **graceful shutdown** so the operator restarts and picks up the new profile. |

### Pattern from cluster-machine-approver PR #286

1. **Scheme:** Add `configv1.AddToScheme(scheme)` so the client can read `config.openshift.io/v1` APIServer.
2. **Client:** Create a `client.Client` (e.g. from manager rest config or a dedicated config) **before** building the manager.
3. **Fetch profile at startup:** `tlsSecurityProfileSpec, err := utiltls.FetchAPIServerTLSProfile(ctx, k8sClient)`. On non-OpenShift or missing APIServer, this returns an error → operator must handle fallback (e.g. fail fast on OpenShift, or use a default profile when APIServer is not available).
4. **TLSOpt from profile:** `tlsConfig, unsupportedCiphers := utiltls.NewTLSConfigFromProfile(tlsSecurityProfileSpec)`. Log unsupported ciphers if any.
5. **Cancelable context:** `ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())` so the process can be stopped when the profile changes.
6. **Manager options:** Pass `tlsConfig` into **webhook** `TLSOpts` and/or **metrics** `metricsserver.Options.TLSOpts`. cluster-machine-approver also enables SecureServing for metrics (CertDir) and uses the same TLSOpt.
7. **Profile watcher:** After manager is created, instantiate `utiltls.SecurityProfileWatcher{ Client: mgr.GetClient(), InitialTLSProfileSpec: tlsSecurityProfileSpec, OnProfileChange: func(ctx context.Context, old, new configv1.TLSProfileSpec) { cancel() } }` and call `watcher.SetupWithManager(mgr)` so profile changes trigger graceful shutdown.
8. **RBAC:** Ensure the operator has `get`, `list`, `watch` on `apiservers.config.openshift.io` (cluster-scoped).

NRO should follow this same pattern for the **operator** webhook and metrics servers (no in-repo profile resolver or custom mapping; use controller-runtime-common). RTE and the scheduler are **operands** and are handled separately below.

---

## TLS Endpoints Summary

| Component | Container | Port | TLS Mechanism | In scope? |
|-----------|-----------|------|---------------|-----------|
| **NRO** Operator webhook | controller-manager | 9443 | Controller-runtime webhook server | Yes |
| **NRO** Operator metrics | controller-manager | 8080 | Controller-runtime metricsserver (SecureServing) | Yes |
| **Scheduler** (when deployed by NRO) | scheduler | 10259 (typical) | kube-scheduler secure port (flags) | Yes – deployer must pass profile |
| **Scheduler** Controller | controller | 8080 | Plain HTTP (metricsserver, no SecureServing) | No TLS server |
| **RTE** Metrics | rte (DaemonSet) | 2112 | Controller-runtime metricsserver (SecureServing) | Yes |

---

## 1. numaresources-operator (NRO) plan

### Background

NRO has two TLS termination points: the operator webhook (port 9443) and the operator metrics server (port 8080). It also deploys operands that run TLS servers: RTE (metrics on 2112) and the secondary scheduler (kube-scheduler secure port). TLS configuration must be fetched from **apiservers.config.openshift.io/cluster** and applied to the operator’s own servers and passed to operands (RTE, scheduler) when NRO deploys them.

### Current state

**Operator webhook server (`cmd/main.go`)**

```go
WebhookServer: webhook.NewServer(webhook.Options{
    Port:    params.webhookPort,
    TLSOpts: webhookTLSOpts(params.enableHTTP2),
}),
```

`webhookTLSOpts` only sets **NextProtos** (HTTP/1.1 vs HTTP/2). **No** MinVersion, CipherSuites, or CurvePreferences. The webhook serves HTTPS but relies entirely on Go’s default cipher/curve preferences.

**Operator metrics server (`cmd/main.go`)**

```go
Metrics: metricsserver.Options{
    BindAddress:   params.metricsAddr,
    SecureServing: true,
    CertDir:       "/certs",
},
```

**No TLSOpts.** Certificates are loaded from `/certs`; MinVersion and CipherSuites are never set → Go defaults.

**OpenShift / API usage**

- Repo has `openshift/api` and `openshift/client-go` (config client) in vendor.
- No **library-go** (no apiserver config observer).
- No RBAC for `config.openshift.io/apiservers`.
- No code reading `configv1.APIServer` or `configv1.TLSSecurityProfile`.

**CSV**

- `features.operators.openshift.io/tls-profiles: "false"` → must become `"true"` when compliant.

**Operands**

- RTE: NRO sets `--metrics-mode=httptls` and mounts cert secret; RTE does **not** receive cluster TLS profile (min version, ciphers, curves) from NRO.
- Scheduler: NRO (via deployer manifests) deploys the scheduler pod; scheduler TLS is configured only by container args (flags). NRO does not currently pass cluster-derived TLS flags to the scheduler.

### What’s already good

- Webhook and metrics already use controller-runtime with **TLSOpts** support (can be extended).
- Certificates for metrics from `/certs` (service-serving cert).
- Go 1.24+ in use (ML-KEM support in default curve preferences when profile allows TLS 1.3).
- `openshift/api` and `configv1.TLSProfiles` already in vendor for profile resolution.

### Implementation plan (high level) – use controller-runtime-common

NRO is a controller-runtime–based operator; use **[openshift/controller-runtime-common/pkg/tls](https://github.com/openshift/controller-runtime-common/tree/main/pkg/tls)** as in [cluster-machine-approver PR #286](https://github.com/openshift/cluster-machine-approver/pull/286). Do **not** implement a custom profile resolver or cipher mapping in NRO.

| Phase | Action |
|-------|--------|
| **0** | Ensure Go 1.24+ (already); verify X25519MLKEM768 available when needed. |
| **1** | Add dependency: `github.com/openshift/controller-runtime-common`; add `configv1` to scheme if not already. Add RBAC for `apiservers.config.openshift.io` (get, list, watch). |
| **2** | **Before** creating the manager: create a k8s client (same config as manager), call `utiltls.FetchAPIServerTLSProfile(ctx, k8sClient)` to get `TLSProfileSpec`. Define fallback for non-OpenShift (e.g. fail or use Intermediate). Call `utiltls.NewTLSConfigFromProfile(spec)` to get TLSOpt; log unsupported ciphers. Create cancelable context `ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())`. |
| **3** | Operator webhook: pass the profile-based TLSOpt (from `NewTLSConfigFromProfile`) into `webhook.Options.TLSOpts` together with existing NextProtos logic. |
| **4** | Operator metrics: add **TLSOpts** to `metricsserver.Options` with the same profile-based TLSOpt (MinVersion, CipherSuites; CurvePreferences when controller-runtime-common supports it post openshift/api#2583). |
| **5** | **SecurityProfileWatcher:** After manager creation, register `utiltls.SecurityProfileWatcher{ Client, InitialTLSProfileSpec, OnProfileChange: func(..., old, new) { cancel() } }` and `SetupWithManager(mgr)` so TLS profile changes trigger graceful shutdown and restart with new profile. |
| **6** | RTE: once RTE supports optional TLS profile input (see RTE plan), pass cluster profile via DaemonSet args (or ConfigMap). NRO already watches resources; reconcile RTE when APIServer TLS changes (e.g. same watch or use resolved profile in cache). |
| **7** | Scheduler: when rendering scheduler Deployment, pass cluster-derived TLS flags (e.g. `--tls-min-version`, `--tls-cipher-suites`) from the **same** TLSProfileSpec (convert to kube-scheduler flag format). |
| **8** | CSV: set `features.operators.openshift.io/tls-profiles: "true"`. Remove any hardcoded TLS from deployment scripts. |
| **9** | Tests: unit tests for wiring (e.g. TLSOpts applied); e2e/tls-scanner for compliance. |

### File change summary (NRO)

| Area | Action |
|------|--------|
| `go.mod` / `go.sum` | Add `github.com/openshift/controller-runtime-common`. |
| `cmd/main.go` | Add configv1 to scheme; create client before manager; `FetchAPIServerTLSProfile` → `NewTLSConfigFromProfile`; cancelable context; pass TLSOpt to webhook and metrics; register `SecurityProfileWatcher` with `OnProfileChange: cancel`. |
| RBAC (role, CSV) | Add `apiservers` under `config.openshift.io`. |
| `pkg/objectupdate/rte/`, controller | When RTE supports profile: inject profile into RTE args/ConfigMap; reconcile when APIServer/profile changes. |
| Scheduler manifests / deployer | Add TLS flags to scheduler container args from resolved profile (same spec used for operator TLS). |
| CSV | `tls-profiles: "true"`. |

### Comparison: current vs after

| Endpoint | Current | After |
|----------|---------|--------|
| Operator webhook | Go defaults (only NextProtos set) | Cluster profile (MinVersion, CipherSuites, CurvePreferences) via TLSOpts |
| Operator metrics | Go defaults (no TLSOpts) | Cluster profile via TLSOpts |
| RTE metrics | Go defaults (no profile from NRO) | Cluster profile passed by NRO (when RTE supports it) |
| Scheduler | Deployer args (no cluster profile) | Cluster profile passed by NRO via scheduler args |

---

## 2. scheduler-plugins plan

### Background

scheduler-plugins provides two binaries: the **scheduler** (kube-scheduler with plugins) and the **controller**. TLS/secure serving for the scheduler is entirely inside **k8s.io/kubernetes** (kube-scheduler); the scheduler-plugins repo only registers plugins and calls `app.NewSchedulerCommand()`. The controller serves metrics over **plain HTTP** (no TLS). So: no TLS server code lives in scheduler-plugins; TLS for the scheduler process is configured by **who deploys it** (NRO / deployer) via command-line flags.

### Current state

**Scheduler binary (`cmd/scheduler/main.go`)**

- `main()` calls `app.NewSchedulerCommand(...)` from `k8s.io/kubernetes/cmd/kube-scheduler/app`.
- No TLS or secure-serving code in this repo. All TLS options (e.g. `--secure-port`, `--tls-cert-file`, `--tls-private-key-file`, `--tls-cipher-suites`, `--tls-min-version`) are defined and consumed in upstream Kubernetes.

**Controller binary (`cmd/controller/app/server.go`)**

```go
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    Scheme: scheme,
    Metrics: metricsserver.Options{
        BindAddress: s.MetricsAddr,  // :8080
    },
    ...
})
```

- **No** SecureServing, **no** CertDir, **no** TLSOpts. Metrics are **plain HTTP**. No TLS server in the controller.

**OpenShift / API**

- No `openshift/api` or OpenShift-specific TLS code in scheduler-plugins.

### What’s already good

- No hardcoded TLS in scheduler-plugins; no OpenShift dependency.
- kube-scheduler already supports TLS flags; deployer can pass cluster-derived flags.

### Implementation plan (scheduler-plugins repo)

**No code changes required in scheduler-plugins** for TLS profile consumption. This repo does not implement or hardcode TLS; it delegates to kube-scheduler and to the deployer’s container args.

**Deployer (NRO) responsibility:** When NRO (or deployer) deploys the scheduler pod, it must pass TLS-related flags derived from the cluster TLS profile (e.g. `--tls-min-version`, `--tls-cipher-suites`, and when available curve preferences) so the scheduler process is compliant. See NRO plan Phase 6.

### Optional (scheduler-plugins)

If maintainers want the binary to optionally read the OpenShift TLS profile when running on OpenShift, they could add logic (e.g. in `main`) to read `apiservers.config.openshift.io/cluster` and inject TLS flags before starting the scheduler. This would require adding `openshift/api` and `openshift/client-go`. Not required for compliance as long as NRO passes the profile via args.

### File change summary (scheduler-plugins)

| Area | Action |
|------|--------|
| Repo | **None** for TLS compliance. Optional: add OpenShift TLS profile reader and flag injection. |

### Comparison: current vs after

| Component | Current | After |
|-----------|---------|--------|
| Scheduler binary | TLS from kube-scheduler flags (set by deployer; may not match cluster profile) | Same mechanism; **deployer (NRO)** passes cluster-derived TLS flags → compliant |
| Controller | HTTP only | No change (no TLS server) |

---

## 3. resource-topology-exporter (RTE) plan

### Background

RTE has one TLS termination point: the **metrics server** (port 2112) when `--metrics-mode=httptls`. It uses controller-runtime’s metricsserver with SecureServing and certs from a mounted secret. RTE must remain deployable on **vanilla Kubernetes** and **OpenShift** with **no OpenShift-specific APIs** in the RTE codebase. So: RTE should accept an **optional, platform-agnostic** TLS profile (min version, cipher list, curves); when deployed by NRO on OpenShift, NRO injects the cluster profile into RTE.

### Current state

**Metrics server (`pkg/metrics/server/setup.go`)**

```go
opts := ctrlmetricssrv.Options{
    SecureServing: secureServing,
    BindAddress:   conf.BindAddress(),
    CertDir:       conf.TLS.CertsDir,
    CertName:      conf.TLS.CertFile,
    KeyName:       conf.TLS.KeyFile,
    TLSOpts: []func(*tls.Config){
        WithClientAuth(conf.TLS.WantCliAuth),
    },
}
```

- **Only** `WithClientAuth` is passed. **No** MinVersion, CipherSuites, or CurvePreferences → **Go defaults**.

**TLSConfig struct**

```go
type TLSConfig struct {
    CertsDir    string
    CertFile    string
    KeyFile     string
    WantCliAuth bool
}
```

- No fields for min version, cipher suites, or curve preferences. `NewDefaultTLSConfig()` and `NewDefaultConfig()` only set cert paths and client auth.

**Flags (`pkg/config/flags.go`)**

- `--metrics-certs-dir`, `--metrics-cert-file`, `--metrics-key-file`, `--metrics-want-cli-auth`. **No** TLS profile flags.

**Config file (`pkg/config/cfgdispatch.go`)**

- `topologyExporter.metricsTLS.*` for cert paths and wantCliAuth. **No** profile keys (minTLSVersion, cipherSuites, curvePreferences).

### What’s already good

- Metrics already use controller-runtime metricsserver with **TLSOpts** (can be extended).
- Cert paths and client auth configurable; certificate rotation / serving cert handled by cluster.
- Single Go module; shared TLS utility straightforward if added.

### Implementation plan (RTE – upstream-friendly)

| Phase | Action |
|-------|--------|
| **1** | **Extend TLSConfig** (or add optional profile struct): optional **MinTLSVersion** (string), **CipherSuites** (comma-separated or slice), **CurvePreferences** (optional, when API supports). No OpenShift imports. |
| **2** | **Add flags**: e.g. `--metrics-tls-min-version`, `--metrics-tls-cipher-suites`, `--metrics-tls-curve-preferences` (empty = use documented default). And/or config file keys under `topologyExporter.metricsTLS.*`. |
| **3** | **Cipher/curve/version mapping**: Add internal mapping from OpenSSL-style cipher names and curve names to Go `crypto/tls` constants (same pattern as MetalLB’s internal/tlsconfig). |
| **4** | **Apply in setup.go**: When profile is provided, build TLSOpt that sets `MinVersion`, `CipherSuites`, `CurvePreferences` on `*tls.Config` and append to `TLSOpts`. When not provided, keep current behavior (document as default for backward compatibility on vanilla K8s). |
| **5** | **Tests**: Unit tests for mapping and defaults; verify TLS args in container when set. |

**NRO (operator) responsibility:** Read cluster TLS profile, convert to RTE’s format (min version string, cipher list), inject via RTE container args (or ConfigMap). Reconcile when APIServer TLS changes. See NRO plan Phase 5.

### File change summary (RTE)

| Area | Action |
|------|--------|
| `pkg/metrics/server/setup.go` | Accept optional profile (from TLSConfig or new struct); add TLSOpt that sets MinVersion, CipherSuites, CurvePreferences when provided. |
| `pkg/config/flags.go` | Add `--metrics-tls-min-version`, `--metrics-tls-cipher-suites`, `--metrics-tls-curve-preferences`. |
| `pkg/config/cfgdispatch.go` | Add config keys for optional profile. |
| `pkg/config/defaults.go` | Wire new TLSConfig fields if used. |
| New (e.g. `internal/tlsconfig/` or in metrics/server) | Cipher/curve/version name → Go constant mapping; builder for TLSOpt. |
| Tests | Unit tests for mapping; optional e2e for TLS negotiation. |

### Migration / backward compatibility

- **No TLS profile flags provided:** Current behavior (Go defaults for version/ciphers). Backward compatible on vanilla K8s.
- **Profile provided (e.g. by NRO on OpenShift):** Explicit MinVersion, CipherSuites, CurvePreferences applied; compliant with cluster profile.

### Comparison: current vs after

| Endpoint | Current | After |
|----------|---------|--------|
| RTE metrics | Go defaults (only ClientAuth set) | When profile provided: cluster profile (MinVersion, CipherSuites, CurvePreferences). When not: documented default. |

### Open questions (RTE)

1. **Curve naming:** Dependent on openshift/api PR #2583 stabilizing curve name strings; RTE should use same names for compatibility when NRO passes them.
2. **Default when unset:** Document explicitly (e.g. TLS 1.2 min + safe ciphers, or “Go defaults”) so vanilla K8s deployers know what to expect.

---

## Dependencies and ordering

1. **NRO** can implement operator webhook + metrics (Phases 1–5) using controller-runtime-common as soon as the dependency and RBAC are in place. Scheduler flag injection (Phase 7) uses the same TLSProfileSpec. Depends on **openshift/api** (already in vendor); **openshift/api#2583** for curves when controller-runtime-common adds CurvePreferences.
2. **RTE** plan can be implemented in parallel; NRO’s RTE injection (Phase 6) depends on RTE accepting the optional profile (flags/config).
3. **scheduler-plugins** repo:** No code changes. NRO (deployer) passes TLS flags when deploying scheduler.
4. **Go:** NRO and RTE already on Go 1.24+; X25519MLKEM768 available when TLS 1.3 / curve preferences are used.

---

## Next steps for each component

### numaresources-operator (NRO)

1. **Adopt controller-runtime-common for operator TLS**
   - Add `github.com/openshift/controller-runtime-common` to go.mod; ensure `configv1` is in the scheme.
   - In `cmd/main.go`, before building the manager: create client → `FetchAPIServerTLSProfile` → `NewTLSConfigFromProfile` → cancelable context.
   - Pass the returned TLSOpt into both webhook `TLSOpts` and metrics `metricsserver.Options.TLSOpts`.
   - Register `SecurityProfileWatcher` with `OnProfileChange` calling `cancel()` so profile changes trigger graceful restart.
   - Add RBAC for `apiservers.config.openshift.io` and set CSV `tls-profiles: "true"` when done.
2. **Operands**
   - **Scheduler:** When rendering the scheduler Deployment, convert the same TLSProfileSpec (or APIServer profile) to kube-scheduler flags (`--tls-min-version`, `--tls-cipher-suites`) and set them on the scheduler container.
   - **RTE:** After RTE upstream adds optional TLS profile flags/config, extend NRO’s RTE DaemonSet/ConfigMap to pass cluster profile (min version, ciphers, curves) and reconcile when APIServer TLS changes.

### scheduler-plugins

- **No code changes** in the repo. TLS for the scheduler process is configured by the deployer (NRO). Ensure NRO (or whoever deploys the scheduler) passes cluster-derived TLS flags to the scheduler container.

### resource-topology-exporter (RTE)

1. **Upstream (RTE repo):** Add **optional** TLS profile input (MinTLSVersion, CipherSuites, optionally CurvePreferences) via flags and/or config file; in `pkg/metrics/server/setup.go`, when provided, apply a TLSOpt that sets these on `*tls.Config`. Keep behavior unchanged when no profile is set (documented default). No OpenShift imports.
2. **NRO:** Once RTE supports the above, inject the cluster TLS profile (from the same source NRO uses for its own TLS—APIServer via controller-runtime-common) into RTE’s container args or ConfigMap, and keep it in sync when the cluster profile changes.

---

## References

- [CNF-21765](https://issues.redhat.com/browse/CNF-21765) – Epic
- [OCPSTRAT-2611](https://issues.redhat.com/browse/OCPSTRAT-2611) – Centralized TLS configuration
- [openshift/api#2583](https://github.com/openshift/api/pull/2583) – TLS curves in API
- [openshift/controller-runtime-common/pkg/tls](https://github.com/openshift/controller-runtime-common/tree/main/pkg/tls) – Shared TLS profile fetch and TLSOpt for controller-runtime operators
- [openshift/cluster-machine-approver PR #286](https://github.com/openshift/cluster-machine-approver/pull/286) – Example: use centralized TLS profile (FetchAPIServerTLSProfile, NewTLSConfigFromProfile, SecurityProfileWatcher)
- MetalLB TLS evaluation (CNF-21983) – structure and pattern for this document
- Internal: [TLS_PQC_4.22_CHANGES.md](./TLS_PQC_4.22_CHANGES.md), [TLS_PQC_RTE_UPSTREAM_FRIENDLY.md](./TLS_PQC_RTE_UPSTREAM_FRIENDLY.md), [TLS_PQC_4.22_SCHEDULER_PLUGINS_ANALYSIS.md](./TLS_PQC_4.22_SCHEDULER_PLUGINS_ANALYSIS.md)
