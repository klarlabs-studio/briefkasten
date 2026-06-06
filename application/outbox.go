package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"go.klarlabs.de/briefkasten/domain"
)

// Outbox drives outbound messages through the domain lifecycle over the
// store and sender ports. The application owns the orchestration; the
// domain owns which transitions are legal; infrastructure owns where the
// messages live and how they leave.
type Outbox struct {
	mu     sync.Mutex
	store  domain.OutboxStore
	sender domain.Sender
}

// NewOutbox binds the store and sender.
func NewOutbox(store domain.OutboxStore, sender domain.Sender) *Outbox {
	return &Outbox{store: store, sender: sender}
}

// Enqueue validates and persists a message in queued state, returning its id.
func (o *Outbox) Enqueue(msg domain.OutboundMessage) (string, error) {
	if err := msg.Validate(); err != nil {
		return "", err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("outbox id: %w", err)
	}
	msg.ID = hex.EncodeToString(buf)
	msg.State = "queued"

	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.store.Write(msg); err != nil {
		return "", err
	}
	return msg.ID, nil
}

// Status returns the message with the given id, whatever its state.
func (o *Outbox) Status(id string) (domain.OutboundMessage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.store.Find(id)
}

// Retry moves a failed message back to queued.
func (o *Outbox) Retry(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	msg, err := o.store.Find(id)
	if err != nil {
		return err
	}
	return o.apply(&msg, "RETRY")
}

// Summary returns the outbox ids grouped by lifecycle state.
func (o *Outbox) Summary() (map[string][]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string][]string{}
	for _, state := range domain.OutboxStates {
		ids, err := o.store.List(state)
		if err != nil {
			return nil, err
		}
		if ids == nil {
			ids = []string{}
		}
		out[state] = ids
	}
	return out, nil
}

// ProcessOnce delivers the queued backlog: each message transitions to
// sending, is handed to the Sender, and ends sent or failed. Returns how
// many messages were delivered.
func (o *Outbox) ProcessOnce(ctx context.Context) (int, error) {
	o.mu.Lock()
	ids, err := o.store.List("queued")
	o.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("outbox list: %w", err)
	}

	delivered := 0
	for _, id := range ids {
		o.mu.Lock()
		msg, err := o.store.Find(id)
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

// apply runs a lifecycle event through the domain statechart and persists
// the move. Caller holds the lock.
func (o *Outbox) apply(msg *domain.OutboundMessage, event string) error {
	next, err := domain.Transition(msg.State, event)
	if err != nil {
		return err
	}
	old := msg.State
	msg.State = next
	if err := o.store.Write(*msg); err != nil {
		return err
	}
	return o.store.Remove(old, msg.ID)
}
