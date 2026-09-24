package engine

import "testing"

func TestFakeEngineConformance(t *testing.T) {
	RunConformance(t, func(t *testing.T) Engine { return NewFake() })
}
