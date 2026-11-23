# Feature tracker

Tracks shipped and planned work for `router`. Check the code for current behavior.

| Status | Meaning |
| --- | --- |
| Shipped | Available in the current code |
| Partial | Some of the work is available |
| Proposed | Planned work with a design below |
| Deferred | Set aside for later |

## Where things live

| Area | Path or section |
| --- | --- |
| Router and engine behavior | Code and `conformance/`, which replace the retired M4 through M6 contracts |
| Standalone `router` CLI | `cmd/router`, `internal/routercli/routerctl` |
| Proposed router diagnostics | §1 |
| Proposed harness plugins | §2 |

## Tracker

| ID | Area | Item | Status | Section |
| --- | --- | --- | --- | --- |
| RTR-1 | router | Explain model routing and effective capabilities | Proposed | §1 |
| RTR-2 | integrations | Rich coding agent harness plugins | Proposed | §2 |

## 0. Shipped baseline

- The router accepts and emits multiple API dialects. It supports versioned tool
  boundaries, optional Responses-to-Chat search replacement for streaming and
  non-streaming requests, keyless bearer compatibility, and metadata-only usage
  accounting. The `internal/server` regression tests cover this
  behavior.
- The standalone `router` CLI (`cmd/router`) provides systemd-first daemon lifecycle
  control (`start`, `stop`, `restart`, `status`, `logs`), model listing/picking, offline
  config validation, and accounting usage rollups.

## 1. Explain model routing and effective capabilities (RTR-1)

**Proposed.**

Add `router models explain <model> --ingress responses`. Show the requested name,
the canonical name if an alias matched, the selected provider and upstream dialect,
and whether the route is native or translated. Show the effective capability values
and the source of each override. If resolution fails, show the reason.

Read the explanation from the pinned router snapshot and use the same resolution
and capability functions as request handling. Expose it through the authenticated
diagnostics interface. Return route metadata without credentials or request data.
[TUIOS config origin](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/CONFIGURATION.md#find-where-a-key-comes-from) ·
[route resolution](../internal/router/snapshot.go) ·
[effective capabilities](../internal/router/reasoning.go) ·
[router diagnostics](configuration.md#diagnostics-endpoint)

## 2. Rich coding agent harness plugins (RTR-2)

**Proposed.**

Provide rich plugins for supported coding agent harnesses that consume the live
model catalog directly from `router` (`/v1/models`) instead of relying on static
per-harness model lists.

- Expose model-specific thinking/reasoning toggles, supported effort levels, and
  context window limits derived from router capability metadata.
- Track live `llama.cpp` and Strata servers in real time so loaded local models
  appear with their actual model display name and dynamically updated context
  size as slots or models change.
