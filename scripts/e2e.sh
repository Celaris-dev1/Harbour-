#!/usr/bin/env bash
# End-to-end test of harbourd/harbour using built binaries (never `go run`),
# against a real Postgres. Exercises:
#   1. submit -> approve -> effects -> done (happy path)
#   2. crash mid-run (probe-able tool) -> resume -> done, no double execution
#   3. crash mid-run (no-probe tool) -> needs_review -> resolve -> resume -> done
#   4. Warrant deny (fake Warrant) -> paused -> Warrant allow -> resume -> done
#
# Processes are only ever killed by PID (never pkill/pgrep -f).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DB_URL="postgres://postgres:postgres@localhost:5432/harbour"
# Unique per run so goal ids/names never collide with a previous run's rows
# left in the (persistent, not dropped) harbour database.
RUN_ID="$(date +%s)-$$"
WORK="$(mktemp -d)"
BIN="$WORK/bin"
LOGS="$WORK/logs"
mkdir -p "$BIN" "$LOGS"

PIDS=()
log() { printf '[e2e] %s\n' "$*" >&2; }
cleanup() {
  local rc=$?
  for pid in "${PIDS[@]:-}"; do
    if [ -n "${pid:-}" ] && kill -0 "$pid" 2>/dev/null; then
      log "killing pid $pid"
      kill -9 "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ $rc -ne 0 ]; then
    log "FAILED (exit $rc). Logs in $LOGS"
  else
    rm -rf "$WORK"
  fi
  exit $rc
}
trap cleanup EXIT

# ---------- Postgres ----------
log "checking Postgres"
if ! pg_isready -h localhost -p 5432 -U postgres >/dev/null 2>&1; then
  log "Postgres not responding; starting cluster 16/main"
  pg_ctlcluster 16 main start
  for i in $(seq 1 30); do
    pg_isready -h localhost -p 5432 -U postgres >/dev/null 2>&1 && break
    sleep 1
  done
fi
pg_isready -h localhost -p 5432 -U postgres >/dev/null 2>&1 || { log "Postgres still not up"; exit 1; }

if ! PGPASSWORD=postgres psql -h localhost -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='harbour'" | grep -q 1; then
  log "creating database harbour"
  PGPASSWORD=postgres psql -h localhost -U postgres -c "CREATE DATABASE harbour" >/dev/null
fi

# ---------- Build ----------
log "building binaries"
go build -o "$BIN/" ./cmd/...

harbour() { HARBOUR_URL="$1" "$BIN/harbour" "${@:2}"; }

wait_state() {
  # wait_state URL id want-state timeout-seconds
  local url="$1" id="$2" want="$3" timeout="${4:-15}"
  local start
  start=$(date +%s)
  while true; do
    local state
    state=$(harbour "$url" get "$id" | jq -r '.goal.state // .state // empty')
    if [ "$state" = "$want" ]; then return 0; fi
    if [ $(( $(date +%s) - start )) -ge "$timeout" ]; then
      log "timed out waiting for $id to reach $want (last state: $state)"
      harbour "$url" get "$id" >&2 || true
      return 1
    fi
    sleep 0.3
  done
}

reason_of() { harbour "$1" get "$2" | jq -r '.goal.reason // .reason // empty'; }
effect_id_for_step() { harbour "$1" get "$2" | jq -r ".effects[] | select(.step==$3) | .id"; }

wait_http() {
  local url="$1" timeout="${2:-15}" start
  start=$(date +%s)
  while ! curl -sf "$url" >/dev/null 2>&1; do
    if [ $(( $(date +%s) - start )) -ge "$timeout" ]; then
      log "timed out waiting for $url"
      return 1
    fi
    sleep 0.3
  done
}

DEMO_DIR="$WORK/demo"
mkdir -p "$DEMO_DIR"

########################################################################
log "=== scenario 1: happy path ==="
########################################################################
ADDR1=":8461"
URL1="http://localhost:8461"
HARBOUR_DATABASE_URL="$DB_URL" HARBOUR_ADDR="$ADDR1" HARBOUR_DEMO_DIR="$DEMO_DIR" HARBOUR_TOKEN=tok \
  "$BIN/harbourd" -workers 2 >"$LOGS/d1.log" 2>&1 &
D1=$!; PIDS+=("$D1")
wait_http "$URL1/healthz"

export HARBOUR_TOKEN=tok
G1=$(harbour "$URL1" submit -id e2e-happy-$RUN_ID -name e2e-happy-$RUN_ID -agent demo.writer \
  -input '{"file":"o.txt","lines":["a","b","c"]}' -approve | jq -r .id)
wait_state "$URL1" "$G1" done 20
[ "$(cat "$DEMO_DIR/o.txt" | tr -d '\n')" != "" ] || { log "output file empty"; exit 1; }
log "scenario 1 OK: goal $G1 done, wrote $(wc -l < "$DEMO_DIR/o.txt") lines"

# idempotent resubmit of the same id+spec must return the same goal, 200/201
RESUB=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer tok" -H 'Content-Type: application/json' \
  -d "{\"id\":\"e2e-happy-$RUN_ID\",\"name\":\"e2e-happy-$RUN_ID\",\"agent\":\"demo.writer\",\"created_by\":\"e2e\",\"input\":{\"file\":\"o.txt\",\"lines\":[\"a\",\"b\",\"c\"]}}" \
  "$URL1/v1/goals")
[ "$RESUB" -lt 300 ] || { log "idempotent resubmit failed: $RESUB"; exit 1; }
CONFLICT=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer tok" -H 'Content-Type: application/json' \
  -d "{\"id\":\"e2e-happy-$RUN_ID\",\"name\":\"e2e-happy-$RUN_ID\",\"agent\":\"different-agent\",\"created_by\":\"e2e\",\"input\":{}}" \
  "$URL1/v1/goals")
[ "$CONFLICT" = "409" ] || { log "conflicting goal_id resubmit: got $CONFLICT, want 409"; exit 1; }
log "scenario 1b OK: goal_id idempotency + conflict via API"

kill -9 "$D1"; wait "$D1" 2>/dev/null || true

########################################################################
log "=== scenario 2: crash mid-run (probe-able tool) -> resume -> done ==="
########################################################################
ADDR2=":8462"
URL2="http://localhost:8462"
HARBOUR_DATABASE_URL="$DB_URL" HARBOUR_ADDR="$ADDR2" HARBOUR_DEMO_DIR="$DEMO_DIR" HARBOUR_TOKEN=tok \
  HARBOUR_TEST_CRASH_AFTER_EFFECT_STEP=1 \
  "$BIN/harbourd" -workers 1 >"$LOGS/d2-crash.log" 2>&1 &
D2A=$!; PIDS+=("$D2A")
wait_http "$URL2/healthz"
G2=$(harbour "$URL2" submit -id e2e-crash-$RUN_ID -name e2e-crash-$RUN_ID -agent demo.writer \
  -input '{"file":"c.txt","lines":["x","y","z"]}' -approve | jq -r .id)
# The daemon is expected to exit(137) right after step 1's effect.
set +e
wait "$D2A"; RC=$?
set -e
[ "$RC" -ne 0 ] || { log "crashing daemon exited cleanly, expected non-zero"; exit 1; }
log "daemon 2a crashed as expected (exit $RC)"

HARBOUR_DATABASE_URL="$DB_URL" HARBOUR_ADDR="$ADDR2" HARBOUR_DEMO_DIR="$DEMO_DIR" HARBOUR_TOKEN=tok \
  "$BIN/harbourd" -workers 1 >"$LOGS/d2-resume.log" 2>&1 &
D2B=$!; PIDS+=("$D2B")
wait_http "$URL2/healthz"
wait_state "$URL2" "$G2" done 20
LINES=$(cat "$DEMO_DIR/c.txt" | wc -l)
[ "$LINES" -eq 3 ] || { log "expected 3 lines after crash/resume, got $LINES"; exit 1; }
log "scenario 2 OK: goal $G2 done, no double execution ($LINES lines, each once)"
kill -9 "$D2B"; wait "$D2B" 2>/dev/null || true

########################################################################
log "=== scenario 3: crash on a no-probe tool -> needs_review -> resolve -> done ==="
########################################################################
ADDR3=":8463"
URL3="http://localhost:8463"
HARBOUR_DATABASE_URL="$DB_URL" HARBOUR_ADDR="$ADDR3" HARBOUR_DEMO_DIR="$DEMO_DIR" HARBOUR_TOKEN=tok \
  HARBOUR_TEST_CRASH_AFTER_EFFECT_STEP=0 \
  "$BIN/harbourd" -workers 1 >"$LOGS/d3-crash.log" 2>&1 &
D3A=$!; PIDS+=("$D3A")
wait_http "$URL3/healthz"
G3=$(harbour "$URL3" submit -id e2e-review-$RUN_ID -name e2e-review-$RUN_ID -agent demo.echoer \
  -input '{"n":2}' -approve | jq -r .id)
set +e
wait "$D3A"; RC=$?
set -e
[ "$RC" -ne 0 ] || { log "crashing daemon (scenario 3) exited cleanly, expected non-zero"; exit 1; }

HARBOUR_DATABASE_URL="$DB_URL" HARBOUR_ADDR="$ADDR3" HARBOUR_DEMO_DIR="$DEMO_DIR" HARBOUR_TOKEN=tok \
  "$BIN/harbourd" -workers 1 >"$LOGS/d3-resume.log" 2>&1 &
D3B=$!; PIDS+=("$D3B")
wait_http "$URL3/healthz"
wait_state "$URL3" "$G3" paused 20
case "$(reason_of "$URL3" "$G3")" in
  *needs_review*) log "scenario 3: goal paused for needs_review as expected" ;;
  *) log "unexpected pause reason: $(reason_of "$URL3" "$G3")"; exit 1 ;;
esac
EFF=$(effect_id_for_step "$URL3" "$G3" 0)
[ -n "$EFF" ] || { log "could not find effect for step 0"; exit 1; }
harbour "$URL3" resolve "$EFF" committed -result '{"echoed":"by-operator"}' >/dev/null
harbour "$URL3" resume "$G3" >/dev/null
wait_state "$URL3" "$G3" done 20
log "scenario 3 OK: goal $G3 resolved and done"
kill -9 "$D3B"; wait "$D3B" 2>/dev/null || true

########################################################################
log "=== scenario 4: Warrant deny (fake Warrant) -> paused -> allow -> resume -> done ==="
########################################################################
FW_ADDR=":8556"
FW_URL="http://localhost:8556"
DENY_TOOL="fs.append" ADDR="$FW_ADDR" "$BIN/e2e-fakewarrant" >"$LOGS/fakewarrant.log" 2>&1 &
FW=$!; PIDS+=("$FW")
wait_http "$FW_URL/healthz"

ADDR4=":8464"
URL4="http://localhost:8464"
HARBOUR_DATABASE_URL="$DB_URL" HARBOUR_ADDR="$ADDR4" HARBOUR_DEMO_DIR="$DEMO_DIR" HARBOUR_TOKEN=tok \
  WARRANT_URL="$FW_URL" \
  "$BIN/harbourd" -workers 1 >"$LOGS/d4.log" 2>&1 &
D4=$!; PIDS+=("$D4")
wait_http "$URL4/healthz"

G4=$(harbour "$URL4" submit -id e2e-warrant-$RUN_ID -name e2e-warrant-$RUN_ID -agent demo.writer \
  -input '{"file":"w.txt","lines":["only-line"]}' -approve \
  -warrant-token tok-e2e -warrant-svid svid-e2e | jq -r .id)
wait_state "$URL4" "$G4" paused 20
case "$(reason_of "$URL4" "$G4")" in
  *"warrant denied"*) log "scenario 4: goal paused for warrant denial as expected" ;;
  *) log "unexpected pause reason: $(reason_of "$URL4" "$G4")"; exit 1 ;;
esac
[ ! -e "$DEMO_DIR/w.txt" ] || { log "tool ran despite warrant denial"; exit 1; }

log "granting authority via fake Warrant and resuming"
curl -sf -X PUT -d '' "$FW_URL/control" >/dev/null
harbour "$URL4" resume "$G4" >/dev/null
wait_state "$URL4" "$G4" done 20
[ -e "$DEMO_DIR/w.txt" ] || { log "tool never ran after warrant grant"; exit 1; }
log "scenario 4 OK: goal $G4 denied then done after authorization"
kill -9 "$D4" "$FW"; wait "$D4" 2>/dev/null || true; wait "$FW" 2>/dev/null || true

log "all e2e scenarios passed"
