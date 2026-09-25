# Pricing

Harbour is open core. The single-node runtime — `harbourd`'s durable step engine, its HTTP
API, Ledger spooling, Warrant authorization, the provenance approval policy, and every tool
— is free, forever, with no license required. Running many `harbourd` daemons against one
shared Postgres for high availability is core too, as long as each daemon stays at or under
the default worker cap. See the README's "Built vs roadmap" and LICENSING sections for
exactly what that covers.

One *scale-out* feature on top of `harbourd` requires a paid, offline license key. Nothing
is ever deleted or hidden when a license lapses or was never installed: that feature just
turns off (or, briefly, keeps working with a warning — see "Grace period" below) until a
valid license is present again.

## Community — free

- `harbourd` on any deployment (public, private, solo or commercial), up to 2 worker
  goroutines per process
- Many `harbourd` daemons sharing one Postgres, each within the free worker cap
- The `harbour` CLI, the demo agent and tools, Ledger durability, Warrant authorization

## Enterprise — contact sales

Unlocks, on top of Community:

- **Scale-out worker pools.** `harbourd -workers N` for `N` above the free cap of 2 —
  more concurrent step execution per process for higher-throughput deployments.

Custom seat counts, multi-year terms, and support SLAs — contact sales.

## How licensing works

A license is a small JSON document (customer, edition, features, seats, issued/expiry
dates) signed with Ed25519 and handed to you as one base64url token. Set it via
`HARBOUR_LICENSE` (the token itself) or `HARBOUR_LICENSE_FILE` (a path to a file holding
it). Verification is entirely offline: the token is checked against a vendor public key
compiled into the `harbourd` binary. There is no phone-home, no activation server, and no
telemetry — this works in air-gapped environments.

- `harbourd license show` — current state, customer, edition, features, seats, expiry.
- `harbourd license verify <token|file>` — verify a token offline and print what it grants.

**Grace period.** A license that has expired keeps unlocking its feature, with a loud
warning, for 14 days — so a delayed renewal doesn't interrupt anyone's workflow. After that
it reads as expired and the feature turns off (Community features are unaffected).

**Tampered or invalid tokens** are treated exactly like no license at all (Community), with
a warning logged — Harbour fails safe, never open, on a bad token, but it never fails your
build over one either.

See the README's LICENSING section for how a vendor issues keys with `tools/licensegen`.
