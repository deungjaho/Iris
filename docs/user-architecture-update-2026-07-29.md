# User Architecture Update — 2026-07-29

Verbatim persistence of the user's architecture direction. This is the source of truth for `architecture.md` §1–§5.

---

## Iris positioning

Iris is an **optional user-facing interaction surface/assistant**, not Pantheon control-plane truth and not a mandatory routing layer.

## Capability 1

A persistent sandboxed general-assistant session for fragmented chat and temporary tasks such as downloading files, summarizing links, and retrieving specified information. Keep this session useful but bounded; delegate substantial work to clean temporary workers/workspaces so casual context does not contaminate project execution.

## Capability 2

Explicit switching/routing to a named Project Master, allowing the user to communicate requirements and receive status directly; an intermediate Concierge/router may relay only when chosen. Preserve project/run/task identity and original user provenance, make the active mode/project visible, and never silently route or reinterpret.

## Capability 3

Receive push notifications for completion, blockage, decisions, and key checkpoints. Notifications are event-derived views with source/project/run/task/event IDs and acknowledgement state; Iris is not the authoritative task database.

## V1 transport

Connect the user's WeChat using the official WeChat plugin approach referenced by OpenClaw. Keep transport behind an adapter so CLI/other chat channels and later a custom mini-program can replace or coexist with WeChat. Future mini-program is post-V1 and demand-driven.

## Required boundaries/contracts

- manual/direct Project Master access remains possible without Iris
- temporary-task sandbox remains isolated from project workspaces
- idempotent message delivery/dedup
- explicit routing and reversible return to general chat
- bounded queues/history/timeouts
- owner/cancel/join/restart recovery
- secret/log redaction
- authenticated user allowlist because WeChat is an external ingress
- no production service change in this documentation task

## Separation

Distinguish Iris channel/session UI from Concierge orchestration, Portfolio aggregation, Project Master execution, Pantheon registry/event truth, and Beacon notification policy.

## Task scope

Assess how the existing Iris implementation maps to these roles, update roadmap and gaps without inventing features, and send a concise PROJECT_MASTER_REPORT_V1 update.
