package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"go.klarlabs.de/briefkasten/domain"
)

// ErrOutboxBusy reports that another process owns the outbox directory
// right now. It does not mean the outbox is broken — it means the work
// asked for belongs to whoever is holding it.
var ErrOutboxBusy = errors.New("outbox: another process owns this outbox")

// outboxLocker is the cross-process guard a store may offer. It is asked
// for rather than demanded by the domain port: locking is a property of
// where the records live, and an in-memory store has nothing to lock.
//
// The sync.Mutex below only orders goroutines inside one process. The
// server's delivery worker and a `briefkasten send` are two processes over
// one directory, and only the store knows how to keep them apart.
type outboxLocker interface {
	Lock() (release func(), err error)
	TryLock() (release func(), ok bool, err error)
}

// stateReader reads the copy of an id held under one named state. Find
// cannot express that: mid-move the same id exists under two states, and
// recovery has to compare the copies to tell which one is current.
type stateReader interface {
	FindIn(state, id string) (domain.OutboundMessage, error)
}

// Outbox drives outbound messages through the domain lifecycle over the
// store and sender ports. The application owns the orchestration; the
// domain owns which transitions are legal; infrastructure owns where the
// messages live and how they leave.
type Outbox struct {
	mu     sync.Mutex
	store  domain.OutboxStore
	sender domain.Sender
	locker outboxLocker // nil when the store needs no cross-process guard
}

// NewOutbox binds the store and sender.
func NewOutbox(store domain.OutboxStore, sender domain.Sender) *Outbox {
	ob := &Outbox{store: store, sender: sender}
	if l, ok := store.(outboxLocker); ok {
		ob.locker = l
	}
	return ob
}

// hold takes the cross-process lock for the whole of a state-mutating
// operation. The order is always lock-then-mu and never the reverse: the
// delivery worker drops mu around the wire send but keeps the file lock,
// so a goroutine that took mu first and then waited for the file lock
// would deadlock the worker against itself.
func (o *Outbox) hold() (func(), error) {
	if o.locker == nil {
		return func() {}, nil
	}
	return o.locker.Lock()
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

	// Deliberately outside the cross-process lock. That lock serialises
	// state transitions on records that already exist — the reason a
	// concurrent Recover must not decide an in-flight send has died.
	// This id was just drawn from 128 random bits, so no other process
	// can be holding it in any state, and the write itself is atomic.
	// Taking the lock here would only make an email.send wait out an
	// in-progress delivery run — up to the send timeout times the retry
	// budget — which the caller experiences as a hung tool call.
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.store.Write(msg); err != nil {
		return "", err
	}
	return msg.ID, nil
}

// From reports the address mail leaves this outbox from, or "" when the
// transport cannot name one. See domain.SelfAddresser: it is what a
// derived reply excludes from its recipients.
func (o *Outbox) From() string {
	if a, ok := o.sender.(domain.SelfAddresser); ok {
		return a.From()
	}
	return ""
}

// Status returns the message with the given id, whatever its state.
// Reading takes no cross-process lock: records are written atomically, so
// a reader never sees half a record, and a status query must stay
// answerable while a delivery run holds the outbox.
func (o *Outbox) Status(id string) (domain.OutboundMessage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.store.Find(id)
}

// Retry moves a failed message back to queued.
func (o *Outbox) Retry(id string) error {
	release, err := o.hold()
	if err != nil {
		return err
	}
	defer release()

	o.mu.Lock()
	defer o.mu.Unlock()
	msg, err := o.store.Find(id)
	if err != nil {
		return err
	}
	return o.apply(&msg, "RETRY")
}

// Summary returns the outbox ids grouped by lifecycle state. Read-only,
// so it never waits on the outbox lock — see Status.
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
// many messages were delivered. Persistence failures while recording an
// outcome are joined into the returned error — the message itself stays
// recoverable on disk either way.
//
// The outbox lock is held across the wire sends, not just around the state
// writes. A message parked in sending is only meaningful while its sender
// is alive; holding the lock is what lets another process tell "someone is
// delivering this" from "someone died delivering this".
func (o *Outbox) ProcessOnce(ctx context.Context) (int, error) {
	release, err := o.hold()
	if err != nil {
		return 0, err
	}
	defer release()

	o.mu.Lock()
	ids, err := o.store.List("queued")
	o.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("outbox list: %w", err)
	}

	delivered := 0
	var errs []error
	for _, id := range ids {
		o.mu.Lock()
		msg, err := o.store.Find(id)
		if err != nil {
			o.mu.Unlock()
			// Nobody else can move a record while we hold the outbox, so a
			// listed id that will not load is a damaged file, not a race.
			errs = append(errs, fmt.Errorf("outbox read %s: %w", id, err))
			continue
		}
		// Count the attempt before the move to sending so the number
		// survives a crash mid-delivery.
		msg.Attempts++
		if err := o.apply(&msg, "SEND"); err != nil {
			o.mu.Unlock()
			continue
		}
		o.mu.Unlock()

		sendErr := o.sender.Send(ctx, msg)

		o.mu.Lock()
		if sendErr != nil {
			msg.Error = sendErr.Error()
			if err := o.apply(&msg, "FAIL"); err != nil {
				errs = append(errs, fmt.Errorf("outbox record failure of %s: %w", msg.ID, err))
			}
		} else {
			msg.Error = ""
			if err := o.apply(&msg, "SUCCEED"); err == nil {
				delivered++
			} else {
				errs = append(errs, fmt.Errorf("outbox record delivery of %s: %w", msg.ID, err))
			}
		}
		o.mu.Unlock()
	}
	return delivered, errors.Join(errs...)
}

// staleSide names, for every legal transition's (old, new) state pair, the
// side a crash between Write(new) and Remove(old) leaves behind as stale.
var staleSide = map[[2]string]string{
	{"queued", "sending"}: "queued",  // SEND
	{"sending", "sent"}:   "sending", // SUCCEED
	{"sending", "failed"}: "sending", // FAIL
	{"failed", "queued"}:  "failed",  // RETRY
}

// keepOrder breaks a duplicate that nothing else can date. The lifecycle
// is a cycle (failed --RETRY--> queued), so no ranking of states can be
// right in every direction; this one picks the copy furthest from an
// automatic re-send. A message parked in failed costs a human one retry,
// while a stale copy left in queued costs the recipient a second delivery
// of mail they already have.
var keepOrder = []string{"sent", "failed", "sending", "queued"}

// survivor names the copy of a duplicated id that is authoritative.
//
// A duplicate is always the residue of an interrupted apply: Write(new)
// landed, Remove(old) did not. The most recently written copy is therefore
// the true state, and Attempts dates the copies — ProcessOnce increments
// it before every SEND, so more attempts means written later. Where the
// counts tie, a plain pair is decided by the transition table above (the
// stale side loses) and anything wider by keepOrder.
func survivor(byState map[string]domain.OutboundMessage) string {
	top := []string{}
	best := -1
	// Indexed rather than ranged by value: OutboundMessage carries the
	// rendered body, so copying one per state is pure waste.
	for state := range byState {
		switch attempts := byState[state].Attempts; {
		case attempts > best:
			best, top = attempts, []string{state}
		case attempts == best:
			top = append(top, state)
		}
	}
	if len(top) == 1 {
		return top[0]
	}
	if len(top) == 2 {
		if stale, ok := staleSide[[2]string{top[0], top[1]}]; ok {
			return otherThan(top, stale)
		}
		if stale, ok := staleSide[[2]string{top[1], top[0]}]; ok {
			return otherThan(top, stale)
		}
	}
	for _, state := range keepOrder {
		for _, candidate := range top {
			if candidate == state {
				return state
			}
		}
	}
	return top[0]
}

// otherThan picks the side of a pair that is not the stale one.
func otherThan(pair []string, stale string) string {
	if pair[0] == stale {
		return pair[1]
	}
	return pair[0]
}

// Recover repairs the store after an unclean shutdown:
//
//  1. A record that will not decode — a half-written file from a crash —
//     is reported and left untouched. It is somebody's unsent mail; the
//     operator decides, not the recovery.
//  2. A crash between apply's Write and Remove leaves the same id under
//     several states; every copy but the authoritative one is deleted (see
//     survivor for which wins and why).
//  3. A message stranded in sending — the process died mid-delivery — is
//     moved to failed, since whether the wire delivery happened is
//     unknowable. Retry re-queues it deliberately rather than risking a
//     silent duplicate send.
//
// Recovery is the owning process's startup job, so it claims the outbox
// lock without waiting: a directory another process is already working on
// is not stranded, it is busy, and step 3 would otherwise mark that
// process's in-flight message failed behind its back. A busy outbox is
// reported as ErrOutboxBusy and nothing is touched.
func (o *Outbox) Recover() error {
	release, ok, err := o.tryHold()
	if err != nil {
		return fmt.Errorf("outbox recover: %w", err)
	}
	if !ok {
		return fmt.Errorf("outbox recover: %w", ErrOutboxBusy)
	}
	defer release()

	o.mu.Lock()
	defer o.mu.Unlock()

	found, err := o.survey()
	if err != nil {
		return err
	}
	errs := found.errs
	errs = append(errs, o.deduplicate(found)...)
	errs = append(errs, o.failStranded(found)...)
	return errors.Join(errs...)
}

// tryHold claims the outbox only if it is free.
func (o *Outbox) tryHold() (func(), bool, error) {
	if o.locker == nil {
		return func() {}, true, nil
	}
	return o.locker.TryLock()
}

// outboxSurvey is what Recover finds on disk: every decodable record by id
// and state, plus the ids whose copies could not be read at all.
type outboxSurvey struct {
	copies  map[string]map[string]domain.OutboundMessage
	damaged map[string]bool
	errs    []error
}

// survey reads every record once. A failure to enumerate is fatal — a
// partial listing would make one copy of a duplicate look like the only
// one — while a single undecodable record is recorded and skipped.
func (o *Outbox) survey() (*outboxSurvey, error) {
	found := &outboxSurvey{
		copies:  map[string]map[string]domain.OutboundMessage{},
		damaged: map[string]bool{},
	}
	for _, state := range domain.OutboxStates {
		ids, err := o.store.List(state)
		if err != nil {
			return nil, fmt.Errorf("outbox recover: %w", err)
		}
		for _, id := range ids {
			msg, err := o.read(state, id)
			if err != nil {
				found.damaged[id] = true
				found.errs = append(found.errs,
					fmt.Errorf("outbox recover: %s/%s is unreadable and was left in place: %w", state, id, err))
				continue
			}
			if found.copies[id] == nil {
				found.copies[id] = map[string]domain.OutboundMessage{}
			}
			found.copies[id][state] = msg
		}
	}
	return found, nil
}

// read loads one state's copy, falling back to the store's single view of
// the id when the store cannot address a state directly.
func (o *Outbox) read(state, id string) (domain.OutboundMessage, error) {
	if r, ok := o.store.(stateReader); ok {
		return r.FindIn(state, id)
	}
	return o.store.Find(id)
}

// deduplicate leaves exactly one copy of every duplicated id, whatever the
// number of copies: three-way duplicates are reachable by crashing during
// the repair of a two-way one. An id with an unreadable copy is skipped —
// deciding which copy is current means reading all of them.
func (o *Outbox) deduplicate(found *outboxSurvey) []error {
	var errs []error
	for id, byState := range found.copies {
		if found.damaged[id] || len(byState) < 2 {
			continue
		}
		keep := survivor(byState)
		for state := range byState {
			if state == keep {
				continue
			}
			if err := o.store.Remove(state, id); err != nil {
				errs = append(errs, fmt.Errorf("outbox recover %s: %w", id, err))
				continue
			}
			delete(byState, state)
		}
	}
	return errs
}

// failStranded moves what is left in sending to failed. The record is
// re-read: deduplicate may have deleted the copy the survey saw.
func (o *Outbox) failStranded(found *outboxSurvey) []error {
	var errs []error
	for id, byState := range found.copies {
		if found.damaged[id] {
			continue
		}
		if _, ok := byState["sending"]; !ok {
			continue
		}
		msg, err := o.read("sending", id)
		if err != nil {
			errs = append(errs, fmt.Errorf("outbox recover %s: %w", id, err))
			continue
		}
		if msg.State != "sending" {
			errs = append(errs, fmt.Errorf("outbox recover %s: record in sending/ declares state %q", id, msg.State))
			continue
		}
		msg.Error = "delivery interrupted by restart — outcome unknown; retry to re-send"
		if err := o.apply(&msg, "FAIL"); err != nil {
			errs = append(errs, fmt.Errorf("outbox recover %s: %w", id, err))
		}
	}
	return errs
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
