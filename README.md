# Harbour

A durable runtime for long-running agents. `harbourd` owns agent goals the way an OS
owns processes: named, supervised, resumable after a crash, attachable mid-run, with
two-phase commit around every side effect so a retried action is never double-executed.

(The product spec calls it "Harbor". This is a ground-up build against that design;
it does not reuse Prosper's `AutonomousRunner`.)

## Setup

Requirements: Go 1.24+ and Postgres 16 (or Docker to run it).

```sh
scripts/setup.sh            # check prerequisites, build, create the database on local Postgres, write .env
scripts/setup.sh --docker   # same, but start Postgres with docker compose
scripts/setup.sh --no-db    # build only
set -a; . ./.env; set +a    # load the generated config into your shell
```

The script builds `bin/harbour` and `bin/harbourd`, creates the `harbour` database if Postgres is reachable
(override the admin connection with `PG_ADMIN_URL`), and writes a `.env` (mode 0600, gitignored)
with freshly generated tokens and keys. It is safe to re-run: an existing `.env` or database
is never overwritten. It finishes by printing the commands to start Harbour.

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
  ships `echo`, `fs.append` (probe-able), and the `demo.writer` agent. `internal/gatetool`
  ships `gate.verify`: an effect that runs Gate's own verification (`gate run --format json`,
  the real `gate` binary — never imported as a library) on a repo/base/head and records Gate's
  verdict; its stack-receipt links to Gate's own `gate.verdict` receipt for the same run (see
  "Stack-receipts" below).
- **Ledger**: emits `harbour.goal.transition`, `harbour.effect.intent` and
  `harbour.effect.result` records on chain `harbour`. The actor chain is always
  `[human creator, service harbourd/<worker>, agent]`.

## Stack-receipts

Every `harbour.effect.result` record also carries a signed `payload.receipt`: a
`stack-receipt/v1` envelope (`internal/receipt`, self-contained, validated against Ledger's
`docs/receipt-spec.md` conformance vectors copied into `testdata/receipts/`) over the effect's
outcome, signed with a persistent Ed25519 key (`HARBOUR_RECEIPT_KEY`, base64 seed, or a key file
at `HARBOUR_RECEIPT_KEY_FILE`/the user config dir, created on first use, so `signer_key_id` stays
stable across restarts; see `harbourd keys show`). When the effect's args or result identify a
Warrant token (`token_id`) or a
Gate run this effect composed with (`gate.verify`'s `linked_run_id`), the receipt's `links`
point at them, so `ledger incident` can verify the whole chain — who authorised (Warrant) → what
ran (Harbour) → what verified it (Gate) — without trusting Ledger's storage alone.

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

## External goal IDs

`submit` accepts a caller-supplied `id`. A resubmit with the same id and the
same spec (name/agent/input/parent_id) is idempotent and returns the
existing goal; a resubmit with a different spec is a 409. Ledger records
already carried `goal_id` (the actor-chain recorder always sets it from the
goal), so a caller-chosen id shows up there unchanged too.

## Ledger durability

Ledger writes go through `internal/ledger.Spool` (used automatically
whenever `LEDGER_URL` is set): every record is fsync'd to a local file under
`LEDGER_SPOOL_DIR` (default `./harbour-ledger-spool`) before `Record`
returns, so a down or slow Ledger never blocks a caller or drops a write. A
background goroutine retries delivery with backoff and survives process
restarts by re-scanning the spool directory on start.

## Warrant integration

When `WARRANT_URL` is set, every effect is authorized against
[Warrant](../Warrant)'s `POST /v1/authorize` before the tool runs, using the
`warrant_token`/`warrant_svid` supplied with the goal at submit time
(`harbour submit -warrant-token ... -warrant-svid ...`, or `HARBOUR_WARRANT_TOKEN`/
`HARBOUR_WARRANT_SVID`). A deny leaves the effect's idempotency key untouched
(the tool never runs) and pauses the goal with the reason; `harbour resume`
after the operator fixes authority re-authorizes cleanly. See
`internal/warrant` and `SECURITY.md`.

## Provenance policy

Only `operator`-sourced messages are `Trusted`. An agent can flag
`registry.Action.RequiresApproval` on a tool call it derived from
untrusted-sourced content (`registry.UntrustedInputPresent` is a ready-made
check); the worker then records it as `needs_review` — via
`store.RequestApproval`, using the exact same idempotency-key scheme as the
executor — without ever invoking the tool, exactly like an in-doubt
crash-recovered effect with no probe. This is opt-in per agent; see
`SECURITY.md`'s threat model for what that does and doesn't cover.

## Pluggable engine (governance layer)

`internal/engine` defines the durability primitives Harbour's governance
logic (idempotent effects, needs_review, Warrant gating, Ledger receipts) is
expressed against, independent of the Postgres lease loop above: `Engine`
(`ScheduleStep`, `Timer`, `Signal`/`AwaitSignal`) plus `Runtime`, which
implements that governance purely in terms of `Engine`. Two implementations
ship — `Postgres` (the default) and `Fake` (a from-scratch in-process
engine) — and both pass the identical conformance suite in
`internal/engine/conformance.go`. A `temporal` build tag documents why a
closure-based `go.temporal.io/sdk` adapter isn't shipped (see
`internal/engine/temporal.go`) even though the SDK itself downloads and
builds cleanly. This is an additive demonstration layer; the production
`harbourd` runtime above is unchanged.

## End-to-end test

`scripts/e2e.sh` builds real binaries (never `go run`) and drives
submit→approve→effects→done, a real process crash→resume with no double
execution, a probe-less crash→needs_review→resolve→done, and a Warrant
deny→allow→resume, against Postgres (creating the `harbour` database and
starting the local cluster if needed). It's also the last CI step.

## Built vs roadmap

Built: single-node runtime (many workers/daemons may share one Postgres),
external goal ids, durable Ledger spooling, Warrant authorization, the
provenance approval policy, the pluggable-engine governance layer, and
everything above.

Not yet built (from the spec's "fully built version"):
- Fleet control plane: multi-node scheduling and placement, resource accounting, a
  single-pane UI across hundreds of agents.
- Current-authority-per-goal display in the CLI/API (Warrant enforces it; there's no
  UI surfacing it yet).
- Incident-response UX that joins Harbour state with Ledger replay (the records are
  emitted, but there is no combined replay/roll-forward tool).
- Retry policies and backoff for failed tools (a tool error currently fails the goal),
  per-goal timeouts, and scheduled (cron) goals from the `scheduler` source.
- An LLM-backed agent. Only the deterministic demo agent ships.
- AuthN/Z beyond one shared bearer token for goal-level operator actions (any token
  holder can post `operator` messages or resolve any `needs_review` effect); Warrant
  is what gates individual effects.
- LISTEN/NOTIFY for attach (it polls every 300ms today).
- A real `temporal`-tagged Engine (see `internal/engine/temporal.go` for why, and
  what shape it needs).

## LICENSING (Enterprise add-ons)

Harbour is open core. The single-node runtime — durable step engine, HTTP API, Ledger
spooling, Warrant authorization, the demo agent and every tool — is free and always will be;
see [PRICING.md](PRICING.md). Running many `harbourd` daemons against one Postgres (each at
the default worker cap) is core too. One scale-out feature requires a paid, offline license
key: `HARBOUR_LICENSE` (a token) or `HARBOUR_LICENSE_FILE` (a path). Verification is
Ed25519-signature checking against a vendor public key compiled into the binary
(`internal/license`) — no network call, no phone-home, works air-gapped.

Gated feature:

- **Scale-out worker pools.** `harbourd -workers N` with `N` above the free cap of 2 worker
  goroutines per process. Running at or under the cap — including many daemons sharing one
  Postgres for HA — stays free.

States: **none** (no license — core works fully, gated features are off), **valid**,
**grace** (up to 14 days past expiry — gated features keep working with a loud warning),
**expired** (gated features off). An invalid or tampered token is treated as no license,
with a warning — `harbourd` never fails a build over a bad token, and never deletes or hides
data when a feature turns off (it refuses to start with a gated `-workers` value instead).

- `harbourd license show [--json]` / `harbourd license verify <token|file>` — inspect the
  current or a given license, entirely offline.

**Issuing licenses** (vendor only — needs the private signing key, which must never be
committed):

```
go run -tags licensegen ./tools/licensegen keygen --out priv.key      # keep priv.key OFFLINE
go run -tags licensegen ./tools/licensegen issue --key priv.key \
    --customer "Acme Inc" --edition enterprise --seats 10 --days 365 > license.tok
```

`tools/licensegen` is a separate `main` package behind the `licensegen` build tag, so
`go build ./...` and the released `harbourd` binary never include it. After generating a
real keypair, put its **public** key into `internal/license/keys.go`'s `prodPublicKeyB64`
and cut a release; the private key stays with the vendor, offline, always.

The repo also carries a public **dev** keypair (`licensegen issue --key dev`) for tests.
Release builds never trust it; only binaries built with `-tags licensedev` (the e2e harness)
do. Never ship a `licensedev` build.

## License

Harbour is source-available under the [Functional Source License 1.1, Apache 2.0 future license](LICENSE.md) (FSL-1.1-ALv2).

- You can use, modify and self-host it for free, including inside your company and for commercial work.
- You can't offer it, or a derivative of it, as a competing product or service.
- Each release becomes Apache License 2.0 two years after it is published.

Enterprise features need a license key; see [PRICING.md](PRICING.md) and the LICENSING section above. Self-hosted: you run Harbour on your own infrastructure, and no hosted service is contacted.
