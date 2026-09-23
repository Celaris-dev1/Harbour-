package executor

import (
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
