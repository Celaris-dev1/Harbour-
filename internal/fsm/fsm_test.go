package fsm

import "testing"

func TestTransitions(t *testing.T) {
	ok := [][2]State{{Proposed, Approved}, {Approved, Executing}, {Executing, Verifying}, {Verifying, Done}, {Verifying, Failed}, {Executing, Paused}, {Paused, Executing}, {Approved, Cancelled}}
	for _, p := range ok {
		if err := Check(p[0], p[1]); err != nil {
			t.Errorf("expected legal: %v", err)
		}
	}
	bad := [][2]State{{Proposed, Executing}, {Done, Executing}, {Cancelled, Approved}, {Failed, Done}, {Approved, Done}, {Executing, Done}}
	for _, p := range bad {
		if Check(p[0], p[1]) == nil {
			t.Errorf("expected illegal %s->%s", p[0], p[1])
		}
	}
	for _, s := range All {
		if Terminal(s) && len(legal[s]) != 0 {
			t.Errorf("terminal %s has exits", s)
		}
	}
}
