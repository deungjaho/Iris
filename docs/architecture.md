# Iris Architecture — User Positioning & Capability Map

Status: authoritative design reference for Iris V1.
Scope: positioning, capabilities, boundaries, current-implementation mapping, gaps, roadmap.
Source of truth for this document: user architecture update 2026-07-29, persisted verbatim in `docs/user-architecture-update-2026-07-29.md`.
Implementation source of truth: `/home/camt/Work/Iris-lab/server/` (desensitized Git baseline of `/home/camt/Work/Iris`).
This is a documentation artifact. No production service change is implied or required by this file.

---

## 1. Positioning

Iris is an **optional user-facing interaction surface / assistant**.

Iris is **not**:
- the Pantheon control-plane truth
- a mandatory routing layer
- the authoritative task database
- a Project Master
- the Concierge orchestrator
- the Portfolio aggregator
- the Beacon notification policy authority

Direct/manual Project Master access remains possible without Iris. Iris is one ingress among potentially many; nothing in the system is required to flow through Iris.

---

## 2. Capabilities

### Capability 1 — Persistent sandboxed general-assistant session

A persistent, sandboxed general-assistant session for fragmented chat and temporary tasks:
- downloading files
- summarizing links
- retrieving specified information

Contract:
- Keep the session useful but **bounded**.
- Delegate substantial work to clean temporary workers/workspaces so casual context does not contaminate project execution.
- The sandbox is isolated from project workspaces.

### Capability 2 — Explicit switching/routing to a named Project Master

Explicit switching/routing to a named Project Master, allowing the user to:
- communicate requirements to a Project Master
- receive status directly from a Project Master

An intermediate Concierge/router may relay **only when chosen**. Default path is direct.

Contract:
- Preserve project/run/task identity and original user provenance.
- Make the active mode/project visible at all times.
- Never silently route or reinterpret a message.
- Routing is explicit and reversible: the user can return to general chat.

### Capability 3 — Receive push notifications

Receive push notifications for:
- completion
- blockage
- decisions
- key checkpoints

Contract:
- Notifications are **event-derived views** with source/project/run/task/event IDs and acknowledgement state.
- Iris is **not** the authoritative task database; it is a view/projection.
- Event truth lives in Pantheon registry; Beacon owns notification policy.

---

## 3. V1 Transport

- Connect the user's WeChat using the official WeChat plugin approach referenced by OpenClaw (`https://ilinkai.weixin.qq.com`, bot token, long-poll getupdates).
- Keep transport behind an adapter so CLI/other chat channels and later a custom mini-program can replace or coexist with WeChat.
- Future mini-program is **post-V1** and demand-driven. Do not design it now.

---

## 4. Required Boundaries & Contracts

| # | Boundary | Notes |
|---|----------|-------|
| B1 | Manual/direct Project Master access remains possible without Iris | Iris is optional ingress, not a dependency |
| B2 | Temporary-task sandbox isolated from project workspaces | Capability 1 sandbox ≠ project worktrees |
| B3 | Idempotent message delivery / dedup | Network retries must not double-execute |
| B4 | Explicit routing + reversible return to general chat | No silent routing; user can always exit |
| B5 | Bounded queues / history / timeouts | No unbounded memory growth |
| B6 | owner / cancel / join / restart recovery | A run can be owned, cancelled, joined, restarted |
| B7 | Secret / log redaction | No tokens/cookies/sessions in logs or user-facing messages |
| B8 | Authenticated user allowlist | WeChat is external ingress; allowlist is mandatory |
| B9 | No production service change in this documentation task | This file is documentation only |

---

## 5. Separation of Concerns (do not conflate)

| Layer | Owner | Iris role |
|-------|-------|-----------|
| Iris channel/session UI | Iris | Owns |
| Concierge orchestration | Concierge | Iris may invoke when chosen, does not own |
| Portfolio aggregation | Portfolio | Iris does not own |
| Project Master execution | Project Master | Iris routes to, does not own |
| Pantheon registry / event truth | Pantheon | Iris is a view, not truth |
| Beacon notification policy | Beacon | Iris receives pushes per Beacon policy, does not set policy |

---

## 6. Current Implementation Mapping

Source: `/home/camt/Work/Iris-lab/server/` (baseline commit `04ea6c0`).

### 6.1 What exists today

| Component | File | Current behavior |
|-----------|------|------------------|
| Long-poll ingress | `wechat/ilink.go` `GetUpdates` | WeChat ilink API, hardwired |
| Message send | `wechat/ilink.go` `SendMessage` | WeChat only |
| Session manager | `main.go` `sessionManager` | Per-user session list, home + machine sessions |
| Home session | `main.go` `createHomeSession` | Sandbox at `~/.local/share/iris/sandbox`, auto-created, undeletable |
| Bash exec | `main.go` `handleBashCommand` | `!` prefix → local or SSH bash |
| Machine switching | `main.go` `cmdGo`/`goMachineSession`/`goTmuxAgent` | `:go m0/o0/t0` switches to claude sessions on mac/omarchy/tmux |
| Claude bridge | `wechat/claude_bridge.go` | Spawns `claude --print` locally or via SSH |
| Bridge event loop | `wechat/bridge.go` `Run` | stream-json ↔ WeChat messages, permission prompts |
| Markdown escape | `wechat/escape.go` | WeChat-specific underscore escape |
| Message split | `wechat/message.go` | 1500-char segmentation |
| Remote session list | `wechat/remote_sessions.go` | SSH + python, lists claude sessions on mac |
| Tmux agent list | `wechat/tmux_sessions.go` | SSH + python, lists tmux agents |
| Session history | `wechat/session_history.go` | SSH + python, reads claude jsonl |

### 6.2 Mapping to the three capabilities

#### Capability 1 — sandboxed general assistant

| Sub-requirement | Current state | Gap |
|-----------------|---------------|-----|
| Persistent sandboxed session | **Partial**: home session at `~/.local/share/iris/sandbox`, auto-created, undeletable | Sandboxed dir exists; no explicit "bounded" enforcement, no delegation to temporary workers |
| Fragmented chat / temp tasks | **Partial**: home session accepts any text, spawns claude | No concept of "temporary worker/workspace" — claude runs in the sandbox dir, casual context accumulates |
| Download / summarize / retrieve | **Implicit**: claude can do these via tools | No first-class task type; no bounded queue/history/timeout for temp tasks |
| Isolation from project workspaces | **Partial**: sandbox dir is separate from `cfg.Cwd` | No enforcement that sandbox cannot reach project dirs; claude tool access is not scoped |
| Delegate substantial work to clean workers | **Absent** | No worker delegation mechanism; everything runs in the home claude session |

#### Capability 2 — explicit routing to a named Project Master

| Sub-requirement | Current state | Gap |
|-----------------|---------------|-----|
| Explicit switching | **Partial**: `:go`/`@` switches targets | Targets are **machines/sessions** (m0/o0/t0/Iris session N), not **named Project Masters** |
| Named Project Master | **Absent** | No Project Master registry, no project/run/task identity |
| Preserve project/run/task identity | **Absent** | sessionInfo has Label/ID/Cwd only; no project/run/task fields |
| Preserve user provenance | **Partial**: from_user_id is tracked | Not propagated to the routed target as structured provenance |
| Active mode/project visible | **Partial**: `$` and `:status` show machine/session/mode | No "active project" concept, only machine/session |
| Never silently route | **Met**: routing is explicit (`:go`/`@`) | — |
| Reversible return to general chat | **Partial**: `@`/`:go home` returns to home | Works, but "home" is a session, not a routing concept |
| Concierge may relay only when chosen | **Absent** | No Concierge layer exists |

#### Capability 3 — push notifications

| Sub-requirement | Current state | Gap |
|-----------------|---------------|-----|
| Receive push notifications | **Absent** | Iris only polls WeChat inbound; no inbound push channel from Pantheon/Beacon |
| Event-derived views with IDs | **Absent** | No event/project/run/task/event-ID model |
| Acknowledgement state | **Absent** | No ack model |
| Not authoritative task DB | **Met by absence** | Iris has no task DB today; this is a constraint to preserve, not a gap |

### 6.3 Cross-cutting boundaries

| Boundary | Current state | Gap |
|----------|---------------|-----|
| B1 manual PM access without Iris | **Met by absence** | No PM exists yet; preserve this |
| B2 sandbox isolation | **Partial** | Separate dir, but claude tool access not scoped |
| B3 idempotent delivery/dedup | **Absent** | No message ID dedup; GetUpdatesBuf is the only cursor |
| B4 explicit routing + reversible | **Partial** | Routing is explicit; reversibility via `@home` |
| B5 bounded queues/history/timeouts | **Weak** | replyChan(10→50 in reliability branch), MaxSessionsPerUser=20, no history bound, no per-task timeout |
| B6 owner/cancel/join/restart | **Partial** | `:stop` cancels, `:rm` deletes, `--resume` restarts; no owner/join concept, no run/task |
| B7 secret/log redaction | **Partial (post-security-worker)** | Security branch redacts logs; main baseline still leaks |
| B8 authenticated user allowlist | **Partial (post-security-worker)** | Security branch adds AllowedUsers; baseline has none |
| B9 no prod change in docs task | **Met** | This file is docs only |

### 6.4 Transport adapter

| Sub-requirement | Current state | Gap |
|-----------------|---------------|-----|
| WeChat via OpenClaw plugin | **Met** | ilink API client in `wechat/ilink.go` |
| Transport behind adapter | **Absent** | `wechat.Client` is concrete, imported directly by `main.go` and `bridge.go`; no `Transport`/`Channel` interface |
| CLI / other chat coexist | **Absent** | WeChat is the only transport |
| Mini-program post-V1 | **Out of scope** | Not designed |

---

## 7. Gaps (consolidated, no invented features)

Listed as gaps, not designs. Each gap is a candidate for a future task; none is specified here as a feature.

1. **G1 — Transport abstraction missing.** `wechat.Client` is concrete and directly imported. A `Channel`/`Transport` interface is needed before a second transport can coexist. (Eval branch introduced `MessageSender` interface for testability — a partial precedent, but not a transport adapter.)
2. **G2 — No Project Master routing target.** `:go` targets machines/sessions, not named Project Masters. No project/run/task identity model exists.
3. **G3 — No project/run/task/provenance fields.** `sessionInfo` carries Label/ID/Cwd/Mode only.
4. **G4 — No push notification ingress.** Iris only polls WeChat inbound. No inbound channel from Pantheon/Beacon, no event-ID/ack model.
5. **G5 — No temporary-worker delegation for Capability 1.** Home session runs claude in-place; substantial work contaminates casual context. No clean-worker spawn.
6. **G6 — Sandbox tool scoping absent.** Sandbox dir is separate, but claude tool access is not bounded to the sandbox.
7. **G7 — No message dedup/idempotency.** Inbound message IDs exist in the ilink payload (`message_id`) but are not tracked for dedup.
8. **G8 — No run owner/join.** Sessions are per-user single-owner; no multi-user join or ownership transfer.
9. **G9 — Allowlist not yet in baseline.** Security branch has `AllowedUsers`; baseline does not. B8 requires this in baseline before any external ingress rollout.
10. **G10 — Log redaction not yet in baseline.** Security branch redacts; baseline still leaks user messages, assistant text, API bodies.
11. **G11 — No bounded history.** Session history is unbounded in memory; only `MaxSessionsPerUser=20` caps session count, not history size.
12. **G12 — No per-task timeout.** Bash has 30s timeout; claude sessions have no timeout.

---

## 8. Roadmap Direction (sequence, not schedule)

Ordered by dependency. No dates; no feature design.

1. **Land baseline hardening first.** Merge already-audited security + reliability + eval branches into a pre-deploy branch, resolve `main.go`/`bridge.go` conflicts, re-verify. This closes G9, G10 and parts of G5/G11/G12. **Prerequisite for any external-ingress production use.**
2. **Introduce transport adapter (G1).** Extract a `Channel` interface from `wechat.Client` so a second transport can coexist. Reuse the `MessageSender` precedent from the eval branch as a starting seam, but generalize to inbound + outbound + typing + notify.
3. **Introduce project/run/task identity (G3).** Extend `sessionInfo` (or a new routing record) with project/run/task IDs and user provenance. This is the data model prerequisite for Capability 2.
4. **Introduce named Project Master routing (G2).** Add a Project Master registry target distinct from machine/session targets. `:go <pm-name>` routes explicitly; `@home` returns. Preserve B4.
5. **Introduce push notification ingress (G4).** Add an inbound event channel from Pantheon/Beacon with event/project/run/task/event-ID + ack state. Iris renders views; Pantheon remains truth.
6. **Introduce temporary-worker delegation (G5, G6).** Home session can spawn a clean temporary worker for substantial tasks, with bounded scope, then return to casual context. Sandbox tool scoping.
7. **Idempotency, bounded history, timeouts, owner/join (G7, G8, G11, G12).** Cross-cutting reliability contracts.

Mini-program (post-V1, demand-driven) is intentionally not on this roadmap.

---

## 9. What this document does NOT do

- Does not change any production service.
- Does not design features; gaps are listed, not specified.
- Does not schedule work.
- Does not make Iris a control-plane component.
- Does not make Iris authoritative for tasks, events, or notifications.
- Does not design the mini-program.
