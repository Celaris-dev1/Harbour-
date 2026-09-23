# Harbour

A durable runtime for long-running agents. `harbourd` owns agent goals the way an OS
owns processes: named, supervised, resumable after a crash, attachable mid-run, with
two-phase commit around every side effect so a retried action is never double-executed.

(The product spec calls it "Harbor". This is a ground-up build against that design;
it does not reuse Prosper's `AutonomousRunner`.)

## Quickstart

```sh
docker compose up -d postgres
go build -o bin/ ./cmd/...
bin/harbourd -db postgres://postgres:postgres@localhost:5432/harbour &

export HARBOUR_URL=http://localhost:8450
bin/harbour submit -name nightly -agent demo.writer -input '{"file":"out.txt","lines":["a","b"]}'
bin/harbour send nightly -source operator '{"append":"c"}'
bin/harbour approve nightly
bin/harbour attach nightly          # live SSE stream; works from any machine
```

Other commands: `list`, `get`, `pause`, `resume`, `cancel`, `reparent <id> <parent>`,
`messages`, `resolve <effect-id> retry|committed|failed`. Run `harbour` for usage.

Environment: `HARBOUR_DATABASE_URL`, `HARBOUR_ADDR` (default `:8450`), `HARBOUR_TOKEN`
(optional bearer auth; token holders are operators), `LEDGER_URL` / `LEDGER_TOKEN`
(optional; unset = no-op recorder), `HARBOUR_DEMO_DIR`.

## Architecture

```
harbour CLI ──HTTP/SSE──> harbourd ── API (internal/api)
                                   └─ N workers (internal/worker) ──> executor ──> tools
                                              │                          │
                                              └──────── Postgres ────────┘ ──> Ledger
```

- **Goals** (`goals` table): stable ID (`g_…`) and unique name, both addressable.
  States `proposed → approved → executing → verifying → done|failed`, plus `paused`
  (remembers `paused_from`) and `cancelled`. Every transition goes through
  `internal/fsm`'s legal-edge table; illegal ones return 409.
- **Leases**: a worker claims a runnable goal with `FOR UPDATE SKIP LOCKED`, bumping
  `lease_epoch`. It heartbeats at TTL/3. Every write that advances a goal is fenced on
  `(lease_owner, lease_epoch)`, so a stalled or zombie worker cannot commit after
  another worker has taken over. Pause/cancel drop the lease, stopping the owner at
  the next step boundary. A crashed worker's goal becomes claimable after the TTL and
  resumes at `cursor`, the last committed step.
- **Idempotent executor** (`internal/executor`): key =
  `sha256(tool \n canonical_json(args) \n goal_id:step)`. If the key already has a
  committed result, that stored result is returned and the tool is not called.
- **Two-phase commit**: (a) `effects` row `intent` committed; `attempts` bumped
  just before invoking the tool; (b) tool runs, receiving the key so it can forward
  it to external systems; (c) result row `committed|failed`. Recovery of an `intent` row:
  - `attempts = 0`: the tool was never invoked, so it runs.
  - tool implements `Prober`: probe by key. If the effect happened, the result is
    recorded (`via: probe`). If not, it is retried.
  - no probe: `needs_review` and the goal goes to `paused`. An operator runs
    `harbour resolve` and then `resume`. Harbour never blindly retries.
  On resume the worker reuses the tool+args recorded in the intent row, not the
  agent's fresh decision, so a non-deterministic agent cannot fork a second effect.
- **Typed inbound channels**: `messages` carry `source ∈ {operator, peer_agent,
  scheduler, tool_result, retrieved_document}`, sender, channel and `trusted`
  (only `operator` is trusted). Tool results are delivered automatically as
  `tool_result`. Agents see all of them in `StepContext.Messages`. The demo agent acts
  only on trusted operator instructions and ignores injected text from documents or peers.
- **Signals**: approve, pause, resume, cancel, reparent (cycle-checked).
- **Attach**: `GET /v1/goals/{id}/attach` is SSE backed by the Postgres `events` table.
  Any node can serve it, and it supports `Last-Event-ID` / `?after=`.
- **Registry**: implement `registry.Tool` (optionally `registry.Prober`) and
  `registry.Agent` (`Next`, `Verify`), then register them in `cmd/harbourd`. `internal/demo`
  ships `echo`, `fs.append` (probe-able), and the `demo.writer` agent.
- **Ledger**: emits `harbour.goal.transition`, `harbour.effect.intent` and
  `harbour.effect.result` records on chain `harbour`. The actor chain is always
  `[human creator, service harbourd/<worker>, agent]`.

## Tests

```sh
HARBOUR_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/harbour go test -race ./...
```

DB tests skip without the variable. Each test uses its own schema. Crash coverage:
- `cmd/harbourd` `TestProcessKilledMidRunResumesWithoutDoubleExecution`: the real
  daemon process exits (137) after step 3's side effect but before its result row, with
  its lease still held. A second daemon reconciles via the probe and finishes. Each
  line is written exactly once.
- `internal/worker`: crash between effect and result (probe), crash between intent and
  effect, crash on a probe-less tool (goes to needs_review, no retry), 20 repeated
  crash/resume cycles, stale-lease fencing, idempotent replay with reordered args,
  pause/resume/cancel, provenance, 4 concurrent workers × 8 goals with no double execution.

## Built vs roadmap

Built: single-node runtime (many workers/daemons may share one Postgres), everything above.

Not yet built (from the spec's "fully built version"):
- Fleet control plane: multi-node scheduling and placement, resource accounting, a
  single-pane UI across hundreds of agents.
- Warrant integration: authority checks before effects, and current authority shown per goal.
- Incident-response UX that joins Harbour state with Ledger replay (the records are
  emitted, but there is no combined replay/roll-forward tool).
- Retry policies and backoff for failed tools (a tool error currently fails the goal),
  per-goal timeouts, and scheduled (cron) goals from the `scheduler` source.
- An LLM-backed agent. Only the deterministic demo agent ships.
- AuthN/Z beyond one shared bearer token. Any token holder can post `operator` messages.
- LISTEN/NOTIFY for attach (it polls every 300ms today). Ledger writes are
  best-effort after the DB commit, with no outbox.
