// Package fsm defines the goal lifecycle and its legal transitions.
package fsm

import "fmt"

type State string

const (
	Proposed  State = "proposed"
	Approved  State = "approved"
	Executing State = "executing"
	Verifying State = "verifying"
	Done      State = "done"
	Failed    State = "failed"
	Paused    State = "paused"
	Cancelled State = "cancelled"
)

var All = []State{Proposed, Approved, Executing, Verifying, Done, Failed, Paused, Cancelled}

// legal lists every allowed edge. Resume from paused returns to the state the
// goal was paused from, so paused -> {proposed, approved, executing, verifying}.
var legal = map[State][]State{
	Proposed:  {Approved, Paused, Cancelled},
	Approved:  {Executing, Paused, Cancelled},
	Executing: {Verifying, Failed, Paused, Cancelled},
	Verifying: {Done, Failed, Executing, Paused, Cancelled},
	Paused:    {Proposed, Approved, Executing, Verifying, Cancelled},
	Done:      {},
	Failed:    {},
	Cancelled: {},
}

func Valid(s State) bool { _, ok := legal[s]; return ok }

func Terminal(s State) bool { return s == Done || s == Failed || s == Cancelled }

// CanTransition reports whether from -> to is a legal edge.
func CanTransition(from, to State) bool {
	for _, t := range legal[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ErrIllegal is returned for transitions not in the table.
type ErrIllegal struct{ From, To State }

func (e ErrIllegal) Error() string { return fmt.Sprintf("illegal transition %s -> %s", e.From, e.To) }

func Check(from, to State) error {
	if !CanTransition(from, to) {
		return ErrIllegal{from, to}
	}
	return nil
}
