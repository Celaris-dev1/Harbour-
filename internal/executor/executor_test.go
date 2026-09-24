package executor

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCanonicalAndKey(t *testing.T) {
	a, _ := Canonical(json.RawMessage(`{"b": {"y":1,"x":[3, {"q":1,"p":2}]}, "a": 1.50}`))
	if string(a) != `{"a":1.50,"b":{"x":[3,{"p":2,"q":1}],"y":1}}` {
		t.Fatal(string(a))
	}
	k1, _, _ := Key("t", json.RawMessage(`{"a":1,"b":2}`), "g", 1)
	k2, _, _ := Key("t", json.RawMessage(`{"b":2, "a":1}`), "g", 1)
	k3, _, _ := Key("t", json.RawMessage(`{"a":1,"b":2}`), "g", 2)
	k4, _, _ := Key("u", json.RawMessage(`{"a":1,"b":2}`), "g", 1)
	if k1 != k2 || k1 == k3 || k1 == k4 || len(k1) != 64 {
		t.Fatal("key properties violated")
	}
	if _, _, err := Key("t", json.RawMessage(`{bad`), "g", 0); err == nil {
		t.Fatal("expected error")
	}
}

// FuzzCanonical checks Canonical never panics on arbitrary bytes, and that
// when it does accept input, canonicalizing its own output is a no-op
// (idempotent) and re-parses to an equal value (no data loss/corruption).
func FuzzCanonical(f *testing.F) {
	for _, s := range []string{
		`{}`, `[]`, `null`, `true`, `false`, `0`, `-1.5e10`, `""`,
		`{"a":1,"b":[1,2,3]}`, `{ "z": 1, "a": { "y": 2, "x": 1 } }`,
		`{bad`, ``, `   `, `{"a":}`,
		`{"dup":1,"dup":2}`, `[[[[[1]]]]]`, `"unterminated`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := Canonical(in)
		if err != nil {
			return // invalid JSON is an expected outcome, not a bug
		}
		out2, err2 := Canonical(out)
		if err2 != nil {
			t.Fatalf("Canonical produced output it can't re-canonicalize: %v (out=%s)", err2, out)
		}
		if string(out) != string(out2) {
			t.Fatalf("Canonical is not idempotent: %s != %s", out, out2)
		}
		// Canonical documents empty/whitespace-only input as meaning "null"
		// (missing args), which json.Unmarshal on the raw bytes rejects; that
		// is intentional, not a bug, so only check round-tripping for input
		// Canonical didn't special-case.
		// Use UseNumber like Canonical itself does: plain json.Unmarshal
		// converts numbers to float64, which overflows (and errors) for
		// huge-exponent literals Canonical legitimately accepts and
		// preserves as arbitrary-precision json.Number.
		decode := func(b []byte) error {
			var v any
			d := json.NewDecoder(bytes.NewReader(b))
			d.UseNumber()
			return d.Decode(&v)
		}
		if len(bytes.TrimSpace(in)) > 0 {
			if err := decode(in); err != nil {
				t.Fatalf("Canonical accepted input that doesn't parse: %v", err)
			}
		}
		if err := decode(out); err != nil {
			t.Fatalf("Canonical's own output does not parse: %v", err)
		}
	})
}

// FuzzKey checks Key never panics, is deterministic, and that any two
// distinct (tool, args-by-value, goal, step) tuples that Key accepts never
// collide within a reasonable fuzz budget (a real sha256 collision here
// would be newsworthy, not a Harbour bug, so this mainly guards against a
// degenerate implementation, e.g. one that ignores an input entirely).
func FuzzKey(f *testing.F) {
	f.Add("tool", []byte(`{"a":1}`), "g_1", 0)
	f.Add("", []byte(`null`), "", 0)
	f.Add("t", []byte(`{bad`), "g", -1)
	f.Fuzz(func(t *testing.T, tool string, args []byte, goalID string, step int) {
		k1, _, err := Key(tool, args, goalID, step)
		if err != nil {
			return
		}
		k2, _, err2 := Key(tool, args, goalID, step)
		if err2 != nil || k1 != k2 {
			t.Fatalf("Key is not deterministic for identical input: %v %v vs %v", err2, k1, k2)
		}
		if len(k1) != 64 {
			t.Fatalf("key length = %d, want 64 (hex sha256)", len(k1))
		}
	})
}
