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

## Tracker

| ID | Area | Item | Status | Section |
| --- | --- | --- | --- | --- |
| RTR-1 | router | Explain model routing and effective capabilities | Proposed | §1 |

## 0. Shipped baseline

- The router accepts and emits multiple API dialects. It supports versioned tool
  boundaries, optional Responses-to-Chat search replacement for streaming and
  non-streaming requests, keyless bearer compatibility, and metadata-only usage
  accounting. The conformance suite and `internal/server` regression tests cover this
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
