# How NRO Deploys the Scheduler and Applies TLS Profile

This document describes how the numaresources-operator (NRO) deploys the topology-aware scheduler (scheduler-plugins) and how the **cluster TLS profile** would be applied to that scheduler. (Scheduler TLS is planned; this describes the mechanism.)

---

## 1. Current deployment flow

NRO deploys the scheduler when a `NUMAResourcesScheduler` instance exists and the operator is started with `--enable-scheduler=true`.

### Components

| Piece | Location | Role |
|-------|----------|------|
| **Manifests** | `pkg/numaresourcesscheduler/manifests/` | Base Deployment, ConfigMap, RBAC, etc. Loaded at startup. |
| **Reconciler** | `internal/controller/numaresourcesscheduler_controller.go` | `NUMAResourcesSchedulerReconciler` watches `NUMAResourcesScheduler` and syncs scheduler resources. |
| **Updates** | `pkg/objectupdate/sched/sched.go` | Helpers that mutate the Deployment before apply: image, ConfigMap ref, env vars, resources. |

### Deployment YAML (base)

The scheduler Deployment (`pkg/numaresourcesscheduler/manifests/yaml/deployment.yaml`) defines a single container:

- **Name:** `secondary-scheduler`
- **Command:** `/bin/kube-scheduler`
- **Args (base):** `--config=/etc/kubernetes/config.yaml`

So the process is **kube-scheduler** (from Kubernetes); TLS for its secure port is controlled by **command-line flags**, not by code in the scheduler-plugins repo.

### Reconcile path

1. `NUMAResourcesSchedulerReconciler.Reconcile` is triggered by changes to `NUMAResourcesScheduler` (or related resources).
2. `syncNUMASchedulerResources`:
   - Normalizes spec, computes replicas, builds config params.
   - Updates **ConfigMap** (scheduler config YAML) via `schedupdate.SchedulerConfig`.
   - Updates **Deployment**:
     - `schedupdate.DeploymentImageSettings(dp, schedSpec.SchedulerImage)`
     - `schedupdate.DeploymentConfigMapSettings(dp, cmName, cmHash)` (volume + annotation)
     - `schedupdate.SchedulerResourcesRequest(dp, instance)`
     - `schedupdate.DeploymentEnvVarSettings(dp, schedSpec)`
     - Log level updates, etc.
   - Applies all objects (Deployment, ConfigMap, Role, RoleBinding, …) with `apply.ApplyObject`.

So today the scheduler container **only** gets `--config=/etc/kubernetes/config.yaml`. Any TLS behaviour is whatever kube-scheduler does by default (or via its config file if it supports TLS there).

---

## 2. How TLS profile would be applied (planned)

Goal: the scheduler’s **secure port** (HTTPS) should use the **same** TLS policy as the rest of the cluster (from `apiservers.config.openshift.io/cluster`), not Go defaults.

### Option A: NRO injects TLS flags into the scheduler Deployment (recommended)

- **Where:** Same place other Deployment updates happen: inside `syncNUMASchedulerResources`, using a new helper in `pkg/objectupdate/sched`.
- **Data source:** The cluster TLS profile, same as for the operator’s own webhook/metrics:
  - **On OpenShift:** `apiservers.config.openshift.io/cluster` → effective `TLSProfileSpec` (already implemented in `internal/tlsprofile` for the operator).
  - **Non-OpenShift:** Use the same fallback (e.g. Intermediate) as in `tlsprofile.FetchAPIServerTLSProfile`.

**Steps:**

1. **In the reconciler** (`syncNUMASchedulerResources`):
   - Call `tlsprofile.FetchAPIServerTLSProfile(ctx, r.Client)` to get the current cluster `TLSProfileSpec` (same API as in `cmd/main.go`). The reconciler already has `r.Client` and runs in a context where config.openshift.io is available on OpenShift.
   - Pass that spec into a new update helper.

2. **New helper** in `pkg/objectupdate/sched` (e.g. `DeploymentTLSArgs`):
   - Input: `*appsv1.Deployment`, `configv1.TLSProfileSpec`.
   - Find the scheduler container by name (`MainContainerName` = `"secondary-scheduler"`).
   - Convert the spec to kube-scheduler flag format:
     - **Min version:** e.g. `--tls-min-version=VersionTLS12` (or whatever format kube-scheduler expects; Kubernetes often uses the same or similar names as `configv1.TLSProtocolVersion`).
     - **Cipher suites:** `--tls-cipher-suites=...` comma-separated list from `profile.Ciphers`.
   - **Append** these flags to the container’s `Args` (after `--config=...`), so the final args look like:
     - `--config=/etc/kubernetes/config.yaml`
     - `--tls-min-version=VersionTLS12`
     - `--tls-cipher-suites=TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,...`

3. **When profile changes:**  
   The next time the `NUMAResourcesScheduler` (or any related resource) is reconciled, the reconciler will again fetch the TLS profile and call `DeploymentTLSArgs`. So when the admin changes the cluster TLS profile (e.g. in APIServer), the next reconciliation will update the scheduler Deployment with new args and the scheduler pods will roll with the new TLS settings.

4. **Optional:** If we want the scheduler to restart as soon as the TLS profile changes (without waiting for another reconcile of the CR), we can either:
   - Rely on the existing **TLS profile watcher** in the operator: when the profile changes, the operator restarts (we already do that for the operator’s own TLS). After restart, the next reconciliation will use the new profile for the scheduler Deployment, or
   - Add a watch on `apiservers.config.openshift.io` in the scheduler reconciler so that a profile change triggers an immediate reconcile of the scheduler Deployment.

**Summary:** NRO already “owns” the scheduler Deployment. It would, in the same place it sets image and config today, **also** set TLS args derived from the cluster TLS profile. No change is required in the scheduler-plugins repo; kube-scheduler already supports the TLS flags.

### Option B: Scheduler reads the profile itself

- The scheduler-plugins binary could, when running on OpenShift, read `apiservers.config.openshift.io/cluster` and add TLS flags before starting kube-scheduler. That would require adding OpenShift API/client deps to scheduler-plugins and is **not** required for compliance as long as NRO (the deployer) passes the flags (Option A).

---

## 3. End-to-end picture

```
┌─────────────────────────────────────────────────────────────────┐
│  apiservers.config.openshift.io/cluster (TLS profile)            │
└─────────────────────────────────────────────────────────────────┘
         │
         │ 1. NRO operator startup: FetchAPIServerTLSProfile
         │    → webhook + metrics TLSOpts, SecurityProfileWatcher
         │
         │ 2. NRO scheduler reconciler (Reconcile loop):
         │    FetchAPIServerTLSProfile(ctx, r.Client)
         │    → DeploymentTLSArgs(Deployment, profile)
         │    → append --tls-min-version, --tls-cipher-suites to container args
         ▼
┌─────────────────────────────────────────────────────────────────┐
│  Scheduler Deployment (secondary-scheduler container)            │
│  args: [ --config=/etc/kubernetes/config.yaml,                  │
│          --tls-min-version=VersionTLS12,                        │
│          --tls-cipher-suites=TLS_AES_128_GCM_SHA256,... ]        │
└─────────────────────────────────────────────────────────────────┘
         │
         ▼
  kube-scheduler process uses these flags for its secure port (HTTPS).
```

---

## 4. Implementation checklist (when tackling scheduler TLS)

- [ ] Add `DeploymentTLSArgs(dp *appsv1.Deployment, profile configv1.TLSProfileSpec)` in `pkg/objectupdate/sched/sched.go` that appends `--tls-min-version` and `--tls-cipher-suites` to the main container’s `Args`.
- [ ] In `syncNUMASchedulerResources`, call `tlsprofile.FetchAPIServerTLSProfile(ctx, r.Client)` (handle non-OpenShift: same fallback as elsewhere), then `schedupdate.DeploymentTLSArgs(r.SchedulerManifests.Deployment, tlsSpec)`.
- [ ] Confirm kube-scheduler flag names and value format for your Kubernetes version (e.g. `--tls-min-version`, `--tls-cipher-suites`).
- [ ] Optional: ensure scheduler Deployment is reconciled when APIServer TLS profile changes (e.g. watch APIServer in the scheduler reconciler, or rely on operator restart + normal reconcile).
- [ ] Tests: unit test for `DeploymentTLSArgs`; e2e or manual check that the scheduler pod gets the expected args and that TLS matches cluster profile.

This keeps TLS behaviour consistent: **one source of truth** (APIServer TLS profile), and NRO applies it both to its own servers and to the scheduler it deploys.
