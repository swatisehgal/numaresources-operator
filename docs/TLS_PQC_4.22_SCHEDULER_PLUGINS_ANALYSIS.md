# TLS Consistency & PQC Readiness for OCP 4.22 – scheduler-plugins Analysis

This document is a **TLS/PQC 4.22 analysis** for the [openshift-kni/scheduler-plugins](https://github.com/openshift-kni/scheduler-plugins) repository, using the same initiative context as the [NRO TLS change analysis](./TLS_PQC_4.22_CHANGES.md).

---

## 1. Repo overview

- **Repository**: [openshift-kni/scheduler-plugins](https://github.com/openshift-kni/scheduler-plugins)  
- **Role**: Out-of-tree scheduler plugins; provides a **kube-scheduler binary** (with plugins) and a **controller** binary. Fork of [kubernetes-sigs/scheduler-plugins](https://github.com/kubernetes-sigs/scheduler-plugins).
- **Deployment on OpenShift**: The **numaresources-operator** deploys the scheduler-plugins **scheduler** image as a secondary scheduler (NUMAResourcesScheduler). The **controller** image may be used elsewhere; NRO focuses on the scheduler Deployment.

---

## 2. Initiative requirements (recap)

- TLS configuration must come from the **central cluster source** (default: **API Server** – `apiservers.config.openshift.io/cluster`), not from hardcoded or Go-default values.
- Applies to **all TLS servers** in the component (and any operands it manages).
- **Server-side TLS only**; client TLS is out of scope.
- **ML-KEM**: Support TLS 1.3 with ML-KEM when the client supports it; use cluster TLS profile so PQC settings are consistent.

---

## 3. Current state of scheduler-plugins

### 3.1 Scheduler binary (`cmd/scheduler`)

| Aspect | Finding |
|--------|--------|
| **Entrypoint** | `main()` calls `app.NewSchedulerCommand(...)` from **`k8s.io/kubernetes/cmd/kube-scheduler/app`**. No custom TLS or secure-serving code in scheduler-plugins. |
| **TLS / secure serving** | Handled entirely by **upstream Kubernetes** kube-scheduler: secure port, TLS cert/key, and TLS options (e.g. `--secure-port`, `--tls-cert-file`, `--tls-private-key-file`, `--tls-cipher-suites`, and in some versions `--tls-min-version`) are defined and consumed in `k8s.io/kubernetes`. |
| **Hardcoded TLS in this repo?** | **No.** Scheduler-plugins does not set TLS config in code; it only registers plugins and delegates to `NewSchedulerCommand`. |
| **Who configures TLS in practice?** | Whoever **runs** the scheduler container (e.g. NRO, or deployer manifests used by NRO) sets **command-line flags** (or config file). So the effective TLS behavior depends on the **deployment layer** (NRO / [k8stopologyawareschedwg/deployer](https://github.com/k8stopologyawareschedwg/deployer)), not on scheduler-plugins itself. |

So for the **scheduler binary**, this repo does not contain application-level TLS logic. Compliance with the cluster TLS profile depends on the **deployer** passing the right TLS flags (from API Server profile) when starting the scheduler.

### 3.2 Controller binary (`cmd/controller`)

| Aspect | Finding |
|--------|--------|
| **Entrypoint** | `main()` calls `app.Run(options)`; `app` is `sigs.k8s.io/scheduler-plugins/cmd/controller/app`. |
| **Manager / metrics** | Uses `ctrl.NewManager` with `metricsserver.Options{BindAddress: s.MetricsAddr}` (default `:8080`). **No** `SecureServing`, **no** `CertDir`, **no** `TLSOpts`. |
| **TLS server?** | **No.** Metrics are served over **plain HTTP**. There is no TLS server in the controller. |
| **TLS/PQC impact** | No server-side TLS to align with cluster profile today. If SecureServing is added later, the same pattern as NRO (cluster profile → TLSOpts) would apply. |

So the **controller** is currently out of scope for “TLS server consistency” because it does not expose a TLS server.

### 3.3 OpenShift-specific code (`pkg-kni`)

- **Contents**: `pkg-kni/features`, `pkg-kni/knidebug` (from API listing). No TLS or secure-serving code identified.
- **Dependencies**: `go.mod` does **not** include `openshift/api` or `openshift/library-go`; the repo is effectively upstream Kubernetes + plugins + small KNI additions.

---

## 4. Where TLS actually gets configured (scheduler)

When the **scheduler-plugins scheduler** runs on OpenShift:

1. **NRO** (numaresources-operator) deploys the secondary scheduler via manifests that ultimately come from **k8stopologyawareschedwg/deployer** and NRO’s own `pkg/numaresourcesscheduler/manifests`.
2. The **scheduler pod** runs the scheduler-plugins image with **args** (and possibly a **config file**) that define how kube-scheduler starts, including TLS-related flags.
3. If those args do **not** include cluster-derived TLS settings (e.g. `--tls-min-version`, `--tls-cipher-suites` from API Server profile), the scheduler will use **kube-scheduler’s defaults** (or whatever is in the config file), which does **not** satisfy “obtain TLS from central config.”

So for **OCP 4.22 TLS consistency**:

- **scheduler-plugins repo**: No code changes are **required** for TLS profile consumption, because it does not implement or hardcode TLS; it relies on kube-scheduler and on the deployer’s flags.
- **Deployer / NRO**: Should ensure the **scheduler Deployment** passes TLS-related flags (and/or config) derived from **API Server** `tlsSecurityProfile` (e.g. min version, cipher suites, and later curves when the API exists). That work belongs in **numaresources-operator** (and possibly [k8stopologyawareschedwg/deployer](https://github.com/k8stopologyawareschedwg/deployer)) as already outlined in [TLS_PQC_4.22_CHANGES.md](./TLS_PQC_4.22_CHANGES.md).

---

## 5. Optional: cluster TLS profile inside scheduler-plugins

If the **scheduler-plugins** maintainers want the binary to **optionally** read the OpenShift API Server TLS profile when running on OpenShift (instead of relying only on the deployer to pass flags), they could:

1. Add optional logic (e.g. in `cmd/scheduler/main.go` or a small helper) that:
   - Detects OpenShift (e.g. checks for `apiservers.config.openshift.io`).
   - Reads `apiservers.config.openshift.io` / `cluster` and computes the effective TLS profile.
   - Converts the profile to kube-scheduler TLS flags (or a config file) and **prepends** them to the process args (or merges into config) before starting the scheduler.
2. Add dependencies: `openshift/api`, `openshift/client-go` (config client), and optionally `library-go` for profile → TLS mapping.
3. Keep behavior **optional**: if the API is not available (e.g. non-OpenShift), fall back to default flags or whatever the deployer passed.

This would be a **scheduler-plugins** change and is **not** required for the initiative as long as the **deployer (NRO/deployer)** supplies cluster-derived TLS flags.

---

## 6. Controller: if TLS metrics are added later

If the scheduler-plugins **controller** is later built with **TLS for metrics** (e.g. `SecureServing: true` and `CertDir` / `TLSOpts`), then:

- It should **not** rely on Go defaults.
- It should apply the **cluster TLS profile** (API Server) the same way as NRO’s operator (see [TLS_PQC_4.22_CHANGES.md](./TLS_PQC_4.22_CHANGES.md)): read `apiservers.config.openshift.io/cluster`, resolve effective profile, convert to `*tls.Config`, and pass via `metricsserver.Options.TLSOpts`.

No such change is needed while the controller serves metrics over HTTP only.

---

## 7. Summary table

| Component | TLS server? | Hardcoded TLS in repo? | Who must apply cluster profile? |
|-----------|-------------|------------------------|----------------------------------|
| **Scheduler binary** | Yes (kube-scheduler secure port) | No (uses k8s.io/kubernetes) | **Deployer (NRO / deployer)** via scheduler pod args (and/or config). |
| **Controller binary** | No (metrics HTTP only) | N/A | N/A unless TLS metrics are added later. |
| **pkg-kni** | No | No | N/A. |

---

## 8. Recommended actions

### For openshift-kni/scheduler-plugins maintainers

1. **No mandatory code change** for TLS/PQC in this repo, because:
   - The scheduler does not define TLS; kube-scheduler does, and flags are supplied by the deployer.
   - The controller does not expose a TLS server.
2. **Optional**: Implement optional reading of `apiservers.config.openshift.io/cluster` and injection of TLS flags (or config) before starting the scheduler, so that when the binary is run without cluster-derived args, it can still comply on OpenShift.
3. **If** TLS metrics are added to the controller later, implement the same “read API Server → TLSOpts” pattern as in the NRO doc.
4. **Document** that when deployed on OpenShift, the scheduler’s TLS behavior is determined by the **deployment** (e.g. NRO); deployers should pass cluster TLS profile (min version, ciphers, curves) via scheduler args or config.

### For numaresources-operator / deployer

1. Ensure the **scheduler Deployment** (and any config file it uses) gets **TLS settings from the API Server** profile (min version, cipher suites, and when available curves), and pass them as scheduler args (e.g. `--tls-cipher-suites=...`, `--tls-min-version=...`) or via a generated config file. This is already in scope in [TLS_PQC_4.22_CHANGES.md](./TLS_PQC_4.22_CHANGES.md) under “Operands” and “Scheduler.”
2. Add NRO (and deployer, if applicable) to the TLS/PQC checklist for the **scheduler workload** so that the secondary scheduler run by NRO is compliant, even though the scheduler-plugins repo itself does not need to change for that.

---

## 9. References

- Initiative: **OCPSTRAT-2611** (Centralized & enforced TLS); **OCPSTRAT-2361** (PQC/ML-KEM testing); **OCPSTRAT-2916** (APIserver API changes); **HPCASE-180** (tlsAdherence).
- NRO-focused analysis: [TLS_PQC_4.22_CHANGES.md](./TLS_PQC_4.22_CHANGES.md).
- Repo: [openshift-kni/scheduler-plugins](https://github.com/openshift-kni/scheduler-plugins).
- Kubernetes scheduler TLS: [Hardening Guide - Scheduler Configuration](https://kubernetes.io/docs/concepts/security/hardening-guide/scheduler/) (e.g. `--tls-cipher-suites`).
