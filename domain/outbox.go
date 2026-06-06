package domain

import (
	"fmt"

	"go.klarlabs.de/statekit"
)

// OutboxStates are the lifecycle states an outbound message moves through.
var OutboxStates = []string{"queued", "sending", "sent", "failed"}

// newOutboxMachine models the message lifecycle as a statechart. The
// machine — not ad-hoc ifs — decides which transitions are legal:
//
//	queued -SEND-> sending -SUCCEED-> sent (final)
//	                       -FAIL---> failed -RETRY-> queued
func newOutboxMachine(initial string) (*statekit.Interpreter[struct{}], error) {
	machine, err := statekit.NewMachine[struct{}]("outbox").
		WithInitial(statekit.StateID(initial)).
		State("queued").On("SEND").Target("sending").Done().
		State("sending").
		On("SUCCEED").Target("sent").
		On("FAIL").Target("failed").Done().
		State("sent").Final().Done().
		State("failed").On("RETRY").Target("queued").Done().
		Build()
	if err != nil {
		return nil, fmt.Errorf("outbox machine: %w", err)
	}
	interp := statekit.NewInterpreter(machine)
	interp.Start()
	return interp, nil
}

// Transition validates a lifecycle event against the statechart and
// returns the resulting state. An event that does not move the machine
// is illegal.
func Transition(from, event string) (string, error) {
	interp, err := newOutboxMachine(from)
	if err != nil {
		return "", err
	}
	interp.Send(statekit.Event{Type: statekit.EventType(event)})
	to := string(interp.State().Value)
	if to == from {
		return "", fmt.Errorf("outbox: illegal transition %s --%s-->", from, event)
	}
	return to, nil
}
