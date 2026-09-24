//go:build temporal

// Package engine, temporal-tagged: intentionally does not implement Engine
// against go.temporal.io/sdk. Why, so the next person doesn't re-litigate
// this without the context:
//
//   - `go list -m -versions go.temporal.io/sdk` and `go get
//     go.temporal.io/sdk@v1.34.0` both succeed from this environment (the
//     module resolves and downloads cleanly), so the "only if it downloads
//     fine" condition is met on the dependency side.
//   - The blocker is architectural, not availability: Engine.ScheduleStep
//     takes an arbitrary Go closure (StepFunc) as the unit of durable work.
//     Temporal's actual durability guarantee comes from Activities, which
//     must be named, registered with a Worker ahead of time, and given
//     serializable (not closure) inputs, so the Temporal server can
//     schedule, retry and replay them independently of any particular
//     process. There is no way to hand Temporal a closure captured in the
//     caller's stack and get its durability guarantee for it — accepting
//     one and just running it in-process on ScheduleStep would produce a
//     file that imports go.temporal.io/sdk and compiles, but would not
//     actually be backed by Temporal in any meaningful sense (a crash mid
//     fn() would still lose the in-flight attempt, exactly like calling fn
//     directly with no engine at all). Shipping that would be worse than
//     not shipping it: it would look like a real adapter while silently not
//     providing the guarantee the whole rest of this package exists to
//     prove is engine-portable.
//   - A real adapter needs a different, narrower shape: e.g. Engine gains a
//     variant that takes a registered activity name plus a serializable
//     payload instead of a closure, and ScheduleStep is implemented as
//     Signal-With-Start into a small per-key "step host" workflow that
//     calls workflow.ExecuteActivity and records the outcome, with Timer
//     mapping onto workflow.NewTimer and Signal/AwaitSignal onto Temporal
//     signals directly (those three *do* fit Temporal's model cleanly; only
//     the closure-based ScheduleStep does not). That is a real, scoped
//     follow-up, not a rewrite of Harbour.
//   - This environment also has no reachable Temporal server (localhost:7233
//     is not listening), so even a reshaped adapter could not be integration
//     tested here; internal/engine/conformance.go is written so that once
//     such an adapter exists, wiring it into RunConformance is the only
//     change needed to prove it behaves identically to Postgres and Fake.
//
// Build with -tags temporal to get this file instead of a missing-file
// build error, so CI can still assert the tag is at least wired up; there
// is deliberately no Temporal-backed Engine type here yet.
package engine
