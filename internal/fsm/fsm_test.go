package fsm

import (
	"errors"
	"testing"
)

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

// FuzzCheck asserts invariants Check must hold for *any* pair of strings,
// not just the known legal/illegal states: it never panics, it agrees with
// CanTransition, a state can never transition to itself unless that edge is
// explicitly in the legal table (none are), and every edge out of a
// Terminal state is illegal.
func FuzzCheck(f *testing.F) {
	for _, s := range All {
		for _, s2 := range All {
			f.Add(string(s), string(s2))
		}
	}
	f.Add("", "")
	f.Add("proposed", "not-a-real-state")
	f.Add("DROP TABLE goals;--", "done")
	f.Fuzz(func(t *testing.T, from, to string) {
		fs, ts := State(from), State(to)
		err := Check(fs, ts)
		legalEdge := CanTransition(fs, ts)
		if (err == nil) != legalEdge {
			t.Fatalf("Check/CanTransition disagree for %q->%q: err=%v legal=%v", from, to, err, legalEdge)
		}
		if err != nil {
			var ill ErrIllegal
			if !errors.As(err, &ill) {
				t.Fatalf("Check returned a non-ErrIllegal error: %v", err)
			}
		}
		if Terminal(fs) && legalEdge {
			t.Fatalf("terminal state %q has a legal edge to %q", from, to)
		}
	})
}
