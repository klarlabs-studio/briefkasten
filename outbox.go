package briefkasten

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/felixgeelhaar/statekit"
)

// OutboundMessage is one message in the outbox.
type OutboundMessage struct {
	ID      string   `json:"id"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
	// State is the lifecycle state: queued, sending, sent, failed.
	State string `json:"state"`
	// Error holds the last delivery failure, when State is failed.
	Error string `json:"error,omitempty"`
	// Attempts counts delivery attempts.
	Attempts int `json:"attempts"`
}

// Sender delivers an outbound message. Backends: DirSender (maildir,
// local-first), SMTPSender (real mail).
type Sender interface {
	Send(ctx context.Context, msg OutboundMessage) error
}

// outboxStates are the lifecycle directories under the outbox root.
var outboxStates = []string{"queued", "sending", "sent", "failed"}

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

// transition validates a lifecycle event against the statechart and returns
// the resulting state. An event that does not move the machine is illegal.
func transition(from, event string) (string, error) {
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

// Outbox queues outbound messages and drives them through the lifecycle.
// Messages persist as JSON files under <root>/<state>/<id>.json, so a
// restart resumes exactly where it stopped.
type Outbox struct {
	mu     sync.Mutex
	root   string
	sender Sender
}

// NewOutbox creates the state directories and binds the sender.
func NewOutbox(root string, sender Sender) (*Outbox, error) {
	for _, s := range outboxStates {
		if err := os.MkdirAll(filepath.Join(root, s), 0o755); err != nil {
			return nil, fmt.Errorf("outbox init: %w", err)
		}
	}
	return &Outbox{root: root, sender: sender}, nil
}

// Enqueue validates and persists a message in queued state, returning its id.
func (o *Outbox) Enqueue(msg OutboundMessage) (string, error) {
	if len(msg.To) == 0 {
		return "", errors.New("outbox: message needs at least one recipient")
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("outbox id: %w", err)
	}
	msg.ID = hex.EncodeToString(buf)
	msg.State = "queued"

	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.write(msg); err != nil {
		return "", err
	}
	return msg.ID, nil
}

// Status returns the message with the given id, whatever its state.
func (o *Outbox) Status(id string) (OutboundMessage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.find(id)
}

// Retry moves a failed message back to queued.
func (o *Outbox) Retry(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	msg, err := o.find(id)
	if err != nil {
		return err
	}
	return o.apply(&msg, "RETRY")
}

// ProcessOnce delivers the queued backlog: each message transitions to
// sending, is handed to the Sender, and ends sent or failed. Returns how
// many messages were delivered.
func (o *Outbox) ProcessOnce(ctx context.Context) (int, error) {
	o.mu.Lock()
	files, err := filepath.Glob(filepath.Join(o.root, "queued", "*.json"))
	o.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("outbox list: %w", err)
	}

	delivered := 0
	for _, f := range files {
		id := filepath.Base(f)
		id = id[:len(id)-len(".json")]

		o.mu.Lock()
		msg, err := o.find(id)
		if err != nil {
			o.mu.Unlock()
			continue // raced away
		}
		if err := o.apply(&msg, "SEND"); err != nil {
			o.mu.Unlock()
			continue
		}
		o.mu.Unlock()

		msg.Attempts++
		sendErr := o.sender.Send(ctx, msg)

		o.mu.Lock()
		if sendErr != nil {
			msg.Error = sendErr.Error()
			_ = o.apply(&msg, "FAIL")
		} else {
			msg.Error = ""
			if err := o.apply(&msg, "SUCCEED"); err == nil {
				delivered++
			}
		}
		o.mu.Unlock()
	}
	return delivered, nil
}

// apply runs a lifecycle event through the statechart and persists the move.
// Caller holds the lock.
func (o *Outbox) apply(msg *OutboundMessage, event string) error {
	next, err := transition(msg.State, event)
	if err != nil {
		return err
	}
	old := filepath.Join(o.root, msg.State, msg.ID+".json")
	msg.State = next
	if err := o.write(*msg); err != nil {
		return err
	}
	if err := os.Remove(old); err != nil {
		return fmt.Errorf("outbox move: %w", err)
	}
	return nil
}

// write persists the message into its state directory. Caller holds the lock.
func (o *Outbox) write(msg OutboundMessage) error {
	raw, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return fmt.Errorf("outbox marshal: %w", err)
	}
	path := filepath.Join(o.root, msg.State, msg.ID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("outbox write: %w", err)
	}
	return nil
}

// find loads a message by id from whichever state directory holds it.
// Caller holds the lock.
func (o *Outbox) find(id string) (OutboundMessage, error) {
	if id == "" || id != filepath.Base(id) {
		return OutboundMessage{}, fmt.Errorf("%w: %s", ErrBadID, id)
	}
	for _, s := range outboxStates {
		raw, err := os.ReadFile(filepath.Join(o.root, s, id+".json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return OutboundMessage{}, fmt.Errorf("outbox read: %w", err)
		}
		var msg OutboundMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return OutboundMessage{}, fmt.Errorf("outbox decode: %w", err)
		}
		return msg, nil
	}
	return OutboundMessage{}, fmt.Errorf("%w: %s", ErrBadID, id)
}
