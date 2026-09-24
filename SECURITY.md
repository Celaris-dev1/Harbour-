# Security

## Reporting a vulnerability

Please report suspected vulnerabilities privately rather than in a public
issue: open a GitHub security advisory on this repository, or email the
maintainers listed in the repository metadata. Include reproduction steps
and the affected version/commit. We aim to acknowledge reports within a few
business days.

## Threat model

Harbour is a durable runtime for long-running agents: it decides which
tool-call *attempts* happen (idempotently, exactly-effectively-once) and
records what happened. It is **not** a sandbox for the tools/agents it
runs — a compromised or buggy Tool/Agent registered into `internal/registry`
runs with the process's full ambient authority. The two mechanisms in this
repo that narrow that are:

- **Warrant** (`internal/warrant`, wired through `internal/executor` and
  `internal/engine`): when `WARRANT_URL` is set, every effect must be
  authorized against a token scoped by a Warrant broker before the tool
  runs. Harbour trusts Warrant's allow/deny decision; it does not itself
  enforce scopes. See https://github.com/ (Warrant repo) for its own threat
  model — capability tokens, attenuation, budgets, revocation.
- **Provenance** (`internal/registry.Message.Trusted`,
  `registry.Action.RequiresApproval`): only `operator`-sourced messages are
  `Trusted`. An agent that flags `RequiresApproval` on an action derived
  from untrusted content forces `needs_review` (an operator must approve
  before the tool ever runs) instead of executing it directly. This is
  **opt-in per agent** — Harbour cannot generically know whether an agent's
  tool call was influenced by untrusted content it read; only the agent's
  own logic can decide that. Agents that don't use this hook get no
  protection from it. Treat any content whose `Source` is not `operator`
  (or whose `Trusted` is `false`) as potentially adversarial input when
  writing an agent.

Within that model, what Harbour *does* defend against:

- **Double-executing a side effect.** The idempotency-key + two-phase-commit
  scheme (`internal/executor`) means a retried, resumed, or raced attempt at
  the same step either returns the already-committed result or, if genuinely
  in doubt after a crash, requires an operator (`needs_review`) rather than
  guessing. `internal/engine`'s conformance suite (`RunConformance`)
  re-proves this against every Engine implementation, not just the default
  Postgres one.
- **A stale or zombie worker committing after another worker has taken
  over.** Every write that advances a goal is fenced on
  `(lease_owner, lease_epoch)`; a worker whose lease was stolen (expired, or
  force-expired by clock skew between nodes) gets `ErrLeaseLost` on its next
  write, not a silent double-write. See
  `internal/worker/chaos_test.go`.
- **Losing an audit record because Ledger is unreachable.**
  `internal/ledger.Spool` fsyncs every record to a local file before
  `Record` returns and retries delivery in the background, surviving
  process restarts.
- **An unbounded request body.** Every `harbourd` HTTP request body is
  capped (`internal/api.MaxRequestBody`, 1 MiB).
- **A route added later and accidentally skipping auth.** The auth check
  wraps the entire mux, not individual handlers
  (`internal/api.(*Server).Handler`), and
  `TestAuthRequiredOnEveryRoute` walks the route table to confirm it.

What it does **not** defend against, by design or as known scope:

- `HARBOUR_TOKEN` is a single shared bearer token: any holder can post
  `operator` (trusted) messages, sign any signal, and resolve any
  `needs_review` effect for any goal. There is no per-user identity or
  per-goal authorization at the Harbour layer today (Warrant is the
  intended place for finer-grained authority over *effects*; goal-level
  operator actions are not yet gated by it).
- A malicious `registry.Tool` or `registry.Agent` implementation is fully
  trusted code running in-process; Harbour does not isolate or sandbox it.
- Args passed to a tool are not sanitized by Harbour beyond canonical-JSON
  parsing for the idempotency key; a tool is responsible for validating its
  own arguments (see `internal/demo.FileAppend.path` for an example that
  rejects path traversal).

## Fuzzing

`go test ./internal/fsm/... -fuzz FuzzCheck`,
`go test ./internal/executor/... -fuzz FuzzCanonical` (and `FuzzKey`), and
`go test ./internal/api/... -fuzz FuzzRequestParsing` fuzz the state
machine's transition table, the canonical-JSON/idempotency-key scheme, and
HTTP request-body parsing, respectively. Regressions they find are committed
as seed corpus under each package's `testdata/fuzz/`, so `go test` always
re-runs them.

## Supported versions

This is a pre-1.0 project; security fixes land on the default branch. There
is no separate maintenance branch yet.
