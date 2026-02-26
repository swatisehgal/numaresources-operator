# TLS Profile Consistency – Topology Aware Scheduler Stack (High-Level)

## Goal

Ensure the **Topology Aware Scheduler stack** (NUMAResources Operator [NRO], scheduler-plugins, RTE) is compliant with **TLS profile consistency** for OCP 4.22 and PQC readiness.

**Epic:** [CNF-21765](https://issues.redhat.com/browse/CNF-21765)

---

## Scope

- **Applies to:** All **TLS servers** in the stack:
  - **Operator (NRO):** webhook server, metrics server
  - **Operands:** RTE (metrics over TLS); scheduler (kube-scheduler secure port when deployed by NRO)
- **Server-side only:** TLS **client** settings are out of scope.
- **Central source:** API Server by default (`apiservers.config.openshift.io/cluster`); Kubelet or Ingress only if there is a specific reason.

---

## OCP 4.22 Context

- **Tech Preview:** TLSAdherence and TLSCurvePreferences feature gates; implement and test with **tlsAdherence: Strict** once the API is merged.
- **ML-KEM:** TLS servers must support and offer **ML-KEM** when the client supports it (TLS 1.3 with hybrid ML-KEM). Go 1.24+ and OCP 4.22 provide this; the requirement is to **use the cluster TLS profile** (no hardcoding). When the cluster uses a Modern profile (TLS 1.3), the stack will follow.
- **Do not hardcode TLS 1.3:** Follow the **cluster** profile (in 4.22 default is still Intermediate/TLS 1.2). PQC readiness is achieved by honoring the full profile (versions, ciphers, curves), not by forcing TLS 1.3 everywhere.

---

## Action Required

1. **Remove** all local/hardcoded TLS settings (protocols, ciphers, curves).
2. **Fetch and apply** TLS policy from the central source (API Server default; Kubelet or Ingress only if justified).
3. **Do not rely on Go defaults:** Explicitly set MinVersion, CipherSuites (and CurvePreferences when the API is available).
4. **PQC-ready:** Honor the full configured TLS profile so the component is ready for PQC in one pass.

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | All local or hardcoded TLS configurations (protocols, ciphers, curves) removed from codebase and deployment scripts. |
| 2 | Component fetches and applies TLS policy from the central configuration source (API Server default). |
| 3 | **tls-scanner** re-scan confirms the service endpoint is fully compliant with the current global TLS policy. |
| 4 | Service remains stable, functional, and accessible to legitimate clients after deployment. |
| 5 | Component explicitly respects all TLS profile settings (does not rely on Go defaults). |
| 6 | If using Semgrep: review results for remaining hardcoded TLS (false positives are common). |
| 7 | Functional testing confirms the component accepts **only** permitted TLS profile settings (including custom profiles). |
| 8 | Component is PQC-ready in one pass by adhering to all aspects of the configured TLS profile. |

---

## Repositories Impacted

| Repository | Role |
|------------|------|
| [openshift-kni/numaresources-operator](https://github.com/openshift-kni/numaresources-operator/) | Operator (webhook, metrics); deploys RTE and scheduler; must inject cluster TLS profile into operands. |
| [openshift-kni/scheduler-plugins](https://github.com/openshift-kni/scheduler-plugins) | Scheduler binary (kube-scheduler); TLS configured via flags passed by deployer (NRO). No OpenShift API in repo. |
| [k8stopologyawareschedwg/resource-topology-exporter](https://github.com/k8stopologyawareschedwg/resource-topology-exporter/) | RTE metrics TLS server; upstream-friendly approach: optional profile input (flags/config), NRO injects cluster profile when deploying. |

---

## Implementation Plan / Work Breakdown

### Operator (NRO)

| Area | Current state | Required change |
|------|----------------|------------------|
| **Webhook** | `webhookTLSOpts` only sets NextProtos (HTTP/2). No MinVersion/CipherSuites. | Fetch cluster TLS profile; extend **webhookTLSOpts** to apply profile (MinVersion, CipherSuites, curves when API available). |
| **Metrics** | SecureServing + certs from `/certs`; no TLSOpts → Go defaults. | Add **TLSOpts** derived from cluster TLS profile (same as webhook). |
| **CSV** | `features.operators.openshift.io/tls-profiles: "false"` | Set to **`"true"`** once operator (and operands) honor cluster profile. |

### Scheduler (scheduler-plugins)

| Component | TLS? | Responsibility |
|-----------|------|----------------|
| **Scheduler binary** | Yes (kube-scheduler secure port). | TLS is configured via **kube-scheduler flags** (e.g. `--tls-min-version`, `--tls-cipher-suites`). **NRO (or deployer)** must pass cluster-derived TLS flags when deploying the scheduler pod. No change inside scheduler-plugins repo for TLS logic. |
| **Controller** | No. | Metrics served over **HTTP only** (`BindAddress` only; no SecureServing). No TLS server → out of scope. |

### RTE (operand of NRO)

| Area | Current state | Required change |
|------|----------------|------------------|
| **Metrics server** | TLS via `--metrics-mode=httptls`; certs from `metrics-certs-dir`, `metrics-cert-file`, `metrics-key-file`. **No** MinVersion/CipherSuites → Go defaults. | **Upstream-friendly:** RTE to support **optional** TLS profile input (e.g. `--metrics-tls-min-version`, `--metrics-tls-cipher-suites` or config). **NRO** to read API Server profile, convert to RTE format, and inject (args or ConfigMap). RTE remains deployable on vanilla Kubernetes; no OpenShift API in RTE. See [TLS_PQC_RTE_UPSTREAM_FRIENDLY.md](./TLS_PQC_RTE_UPSTREAM_FRIENDLY.md). |

---

## References

- [OCPSTRAT-2611](https://issues.redhat.com/browse/OCPSTRAT-2611) – Centralized & enforced TLS configuration
- Inconsistent TLS Profiles support [FAQ](https://docs.google.com/document/d/...) *(link from initiative)*
- [Hint for resolving TLS non-compliance tickets](https://docs.google.com/document/d/...) *(link from initiative)*
- [Configuring TLS security profiles | OpenShift 4.20](https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html/security_and_compliance/tls-security-profiles)
- Internal: [TLS_PQC_4.22_CHANGES.md](./TLS_PQC_4.22_CHANGES.md), [TLS_PQC_RTE_UPSTREAM_FRIENDLY.md](./TLS_PQC_RTE_UPSTREAM_FRIENDLY.md), [TLS_PQC_4.22_SCHEDULER_PLUGINS_ANALYSIS.md](./TLS_PQC_4.22_SCHEDULER_PLUGINS_ANALYSIS.md)
