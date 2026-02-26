# Upstream-friendly TLS profile for RTE (Kubernetes + OpenShift)

This document clarifies how to satisfy **OCP TLS compliance** for the Resource Topology Exporter (RTE) operand while keeping RTE **upstream-friendly**: deployable on **vanilla Kubernetes** and **OpenShift**, with **no OpenShift-specific APIs** in the RTE codebase.

---

## 1. Constraints

| Requirement | Implication |
|-------------|-------------|
| **RTE runs on Kubernetes and OpenShift** | RTE must not depend on `config.openshift.io` or `openshift/api`. No OpenShift-only code paths in RTE. |
| **OCP compliance when deployed by NRO** | When NRO deploys RTE on OpenShift, RTE’s TLS metrics server must honor the **cluster TLS profile** (from API Server). |
| **Explicit TLS, no Go defaults** | RTE must apply MinVersion, CipherSuites (and later CurvePreferences) from configuration when provided, not rely on Go defaults. |

---

## 2. Division of responsibility

| Party | Responsibility |
|-------|----------------|
| **RTE (upstream)** | Accept an **optional, platform-agnostic** TLS profile (min version, cipher list, and later curves). When provided, apply it to the metrics TLS server; when not provided, use a **documented default** so behavior is predictable on vanilla K8s. |
| **NRO (OpenShift operator)** | Read the cluster TLS profile from **API Server** (`apiservers.config.openshift.io/cluster`), convert it to the **generic format** RTE accepts, and **inject** it into RTE (args, env, or ConfigMap). Reconcile when the cluster profile changes. |

RTE stays agnostic of “where” the profile came from; NRO handles OpenShift-specific sourcing and injection.

---

## 3. Upstream-friendly contract: optional TLS profile input

RTE should support an **optional** TLS profile in a **generic** form that any deployer (OpenShift NRO, or a vanilla K8s deployer with a policy) can provide.

### 3.1 Suggested format (no OpenShift types)

- **MinTLSVersion** (string): e.g. `"1.2"`, `"1.3"`, or Go-style `"VersionTLS12"`, `"VersionTLS13"`. Document one canonical form.
- **CipherSuites** (optional list of strings): cipher names in a single, documented format. Prefer **OpenSSL-style** names (e.g. `TLS_AES_128_GCM_SHA256`, `ECDHE-RSA-AES128-GCM-SHA256`) so OpenShift’s profile can be passed through with minimal mapping; RTE converts to Go `crypto/tls` constants internally.
- **CurvePreferences** (optional, later): when curves are part of the TLS profile, add an optional list so RTE can set `tls.Config.CurvePreferences`.

### 3.2 How RTE can accept the profile (pick one or both)

1. **Command-line flags** (easy for NRO to pass from a rendered manifest)
   - e.g. `--metrics-tls-min-version=1.2`  
   - e.g. `--metrics-tls-cipher-suites=TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,...`
   - Optional: `--metrics-tls-curve-preferences=...` when supported.

2. **Config file** (RTE already has config dispatch)
   - Extend the existing config (e.g. `topologyExporter.metricsTLS.*`) with optional keys: `minTLSVersion`, `cipherSuites` (array or comma-separated), and later `curvePreferences`.  
   - A deployer can mount a ConfigMap with that config; RTE reads it at startup.

3. **Environment variables** (alternative to flags)
   - e.g. `RTE_METRICS_TLS_MIN_VERSION`, `RTE_METRICS_TLS_CIPHER_SUITES` (comma-separated).  
   - NRO can set these from a ConfigMap or from the operator’s computed profile.

**Recommendation:** Flags (and/or config file) are the most operator-friendly; NRO can set container args from the effective profile. Config file is useful if the list of ciphers is long.

---

## 4. RTE behavior (upstream)

- **When TLS profile is provided** (via flags, config, or env):  
  RTE builds `tls.Config` with **MinVersion** and **CipherSuites** (and **CurvePreferences** when supported) from that profile. No reliance on Go defaults for those fields. Append these to the existing `TLSOpts` in `pkg/metrics/server/setup.go` (today only `WithClientAuth` is set).

- **When TLS profile is not provided** (vanilla K8s or older deployers):  
  Use a **documented default** (e.g. TLS 1.2 minimum + a safe cipher list, or “use Go defaults” clearly documented). This preserves backward compatibility and keeps vanilla K8s deployments working without change.

- **No OpenShift imports:** RTE must not import `openshift/api` or `config.openshift.io` clients. All profile input is generic strings/lists.

---

## 5. NRO behavior (OpenShift)

- **Read:** Get `apiservers.config.openshift.io` / `cluster` and compute the effective TLS profile (Old/Intermediate/Modern/Custom) as in the main TLS doc.
- **Convert:** Map the profile to the **generic format** RTE expects (min version string, list of cipher names in the format RTE documents). If RTE accepts OpenSSL-style cipher names, NRO can pass the OpenShift profile’s cipher list with minimal or no conversion.
- **Inject:**  
  - **Option A (args):** Set RTE container args: `--metrics-tls-min-version=...`, `--metrics-tls-cipher-suites=...` (and curves when RTE supports them).  
  - **Option B (ConfigMap):** Create a ConfigMap with the profile in RTE’s config format; mount it and point RTE at it (e.g. config file or env).  
- **Reconcile:** When the cluster TLS profile changes, update the RTE DaemonSet (args or ConfigMap) and roll out so RTE pods pick up the new profile.

---

## 6. Current RTE state (from vendored code)

- **`pkg/metrics/server/setup.go`:** Builds controller-runtime metricsserver with `TLSOpts: []func(*tls.Config){ WithClientAuth(...) }`. **MinVersion** and **CipherSuites** are not set → **Go defaults** (not compliant when a profile is required).
- **`TLSConfig`** (metrics): Only `CertsDir`, `CertFile`, `KeyFile`, `WantCliAuth`. No min version or cipher list.
- **Flags** (`pkg/config/flags.go`): `--metrics-certs-dir`, `--metrics-cert-file`, `--metrics-key-file`, `--metrics-want-cli-auth`. No TLS profile flags yet.
- **Config file** (`cfgdispatch.go`): `topologyExporter.metricsTLS.*` for cert paths and wantCliAuth. No profile keys yet.

So the **upstream change** is: extend RTE’s TLS config and server setup to accept optional MinVersion + CipherSuites (and later CurvePreferences) and apply them in `TLSOpts` when present.

---

## 7. Summary: upstream-friendly path

| Step | Owner | Action |
|------|--------|--------|
| 1 | **RTE** | Add **optional** TLS profile input: MinTLSVersion, CipherSuites (and later CurvePreferences) via flags and/or config file (and optionally env). No OpenShift dependency. |
| 2 | **RTE** | In metrics server setup, when profile is provided: map to `tls.Config` (MinVersion, CipherSuites, CurvePreferences) and add to TLSOpts. When not provided: keep documented default for backward compatibility. |
| 3 | **NRO** | Read API Server TLS profile, convert to RTE’s generic format, inject into RTE via args (or ConfigMap). Reconcile on profile change. |
| 4 | **Both** | Document the contract (format of min version and cipher list) so other deployers can use it on vanilla K8s if they wish. |

This keeps RTE deployable and maintainable on **Kubernetes** and **OpenShift**, satisfies **OCP TLS compliance** when NRO deploys RTE, and avoids any OpenShift-only code in RTE.
