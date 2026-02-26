# TLS profile consistency patches (NRO + RTE)

These patches implement centralized TLS profile support for the Topology Aware Scheduler stack (NRO and RTE). The scheduler is tackled separately.

## 1. NRO patch (`nro-tls-profile.patch`)

**Applies to:** this repo (`openshift-kni/numaresources-operator`).

**Summary:**
- Adds `internal/tlsprofile`: fetch APIServer TLS profile, resolve to `TLSProfileSpec`, build `TLSOpt` for controller-runtime, and `SecurityProfileWatcher` to trigger graceful shutdown when the cluster TLS profile changes.
- **cmd/main.go:** Registers `configv1` in the scheme; before creating the manager, creates a client, fetches the TLS profile from `apiservers.config.openshift.io/cluster`, builds a TLSOpt, and uses a cancelable context. Passes the TLSOpt to both the webhook server and the metrics server. Registers the TLS profile watcher; on profile change, the watcher calls `cancel()` so the process restarts and picks up the new profile.
- **RBAC:** Adds `get`, `list`, `watch` on `apiservers.config.openshift.io`.
- **CSV:** Sets `features.operators.openshift.io/tls-profiles: "true"`.

**Apply:**
```bash
git apply patches/nro-tls-profile.patch
# or
patch -p1 < patches/nro-tls-profile.patch
```

If the patch was generated with staged files, the new files `internal/tlsprofile/profile.go` and `internal/tlsprofile/watcher.go` are included in the patch.

## 2. RTE patch (`rte-tls-profile-support.patch`)

**Applies to:** upstream [resource-topology-exporter](https://github.com/k8stopologyawareschedwg/resource-topology-exporter) (clone that repo, then apply).

**Summary:**
- **pkg/metrics/server/setup.go:** Extends `TLSConfig` with optional `MinTLSVersion` and `CipherSuites` (string, comma-separated IANA names). Adds `buildTLSOpts`, `tlsProfileOpt`, `parseTLSVersion`, and `cipherSuiteIDs` so that when a profile is provided, the metrics server sets `MinVersion` and `CipherSuites` on `*tls.Config`. When not set, behavior is unchanged (Go defaults).
- **pkg/config/flags.go:** Adds `--metrics-tls-min-version` and `--metrics-tls-cipher-suites`.
- **pkg/config/cfgdispatch.go:** Adds config keys `topologyExporter.metricsTLS.minTLSVersion` and `topologyExporter.metricsTLS.cipherSuites`.

After RTE merges this (or equivalent) upstream, NRO can pass the cluster TLS profile to the RTE DaemonSet via container args (e.g. `--metrics-tls-min-version=VersionTLS12 --metrics-tls-cipher-suites=...`) and reconcile when the APIServer TLS profile changes.

**Apply (from the root of the resource-topology-exporter repo):**
```bash
git apply /path/to/numaresources-operator/patches/rte-tls-profile-support.patch
# or
patch -p1 < /path/to/numaresources-operator/patches/rte-tls-profile-support.patch
```

## Scheduler

Scheduler TLS is handled by the **deployer (NRO)** when it deploys the scheduler: NRO will pass cluster-derived TLS flags (e.g. `--tls-min-version`, `--tls-cipher-suites`) to the scheduler container. No patch for the scheduler-plugins repo is included here.
