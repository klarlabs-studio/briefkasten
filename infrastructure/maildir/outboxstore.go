package maildir

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.klarlabs.de/briefkasten/domain"
)

// OutboxStore persists outbound messages as JSON files under
// <root>/<state>/<id>.json — a restart resumes exactly where it stopped.
//
// Two protocols keep the directory trustworthy while a process crashes or
// a second one shows up:
//
//	<root>/tmp/<state>.<id>.json  staging area; records are renamed into
//	                              place, never written in place
//	<root>/.lock                  advisory lock held by whichever process
//	                              is currently mutating the outbox
type OutboxStore struct {
	root string
}

// outboxLockName is the advisory lock guarding the whole directory. It
// sits beside the state dirs rather than inside one: what is guarded is
// the outbox as a whole, including moves between states.
const outboxLockName = ".lock"

// NewOutboxStore creates the state directories and the staging area.
func NewOutboxStore(root string) (*OutboxStore, error) {
	for _, s := range append([]string{"tmp"}, domain.OutboxStates...) {
		if err := os.MkdirAll(filepath.Join(root, s), 0o700); err != nil {
			return nil, fmt.Errorf("outbox init: %w", err)
		}
	}
	return &OutboxStore{root: root}, nil
}

// Lock takes the outbox lock, waiting for whoever holds it. Every process
// that mutates outbox state holds it for the duration of the mutation, so
// waiting here is the same as waiting for another briefkasten to finish.
func (s *OutboxStore) Lock() (func(), error) {
	release, _, err := s.acquire(true)
	return release, err
}

// TryLock takes the outbox lock only if it is free; ok reports whether it
// was. Recovery uses this: a directory another process is working on is
// that process's to repair, not ours.
func (s *OutboxStore) TryLock() (release func(), ok bool, err error) {
	return s.acquire(false)
}

// acquire wraps the platform lock primitive. The lock lives on an open
// file descriptor, which the kernel closes when the process ends however
// it ends — a crash cannot leave the outbox locked, so there is no stale
// lock to clean up and no PID to second-guess.
func (s *OutboxStore) acquire(block bool) (func(), bool, error) {
	f, ok, err := acquireLockFile(filepath.Join(s.root, outboxLockName), block)
	if err != nil {
		return nil, false, fmt.Errorf("outbox lock: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	// Releasing twice must be harmless — a release is usually deferred and
	// sometimes also called early — and a failing Close is not actionable:
	// the descriptor, and with it the lock, is gone at process exit anyway.
	var once sync.Once
	return func() { once.Do(func() { _ = f.Close() }) }, true, nil
}

// Write persists the message under its current state, staged through tmp/
// and renamed into place — the protocol Sender already follows for
// delivered mail. Writing in place would leave a truncated, permanently
// undecodable record behind a crash, which reads as a lost message.
func (s *OutboxStore) Write(msg domain.OutboundMessage) error {
	raw, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return fmt.Errorf("outbox marshal: %w", err)
	}
	// The staging name carries the state as well as the id: mid-move the
	// same id legitimately exists under two states at once.
	tmp := filepath.Join(s.root, "tmp", msg.State+"."+msg.ID+".json")
	if err := writeSynced(tmp, raw); err != nil {
		return fmt.Errorf("outbox write: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.root, msg.State, msg.ID+".json")); err != nil {
		return fmt.Errorf("outbox write: %w", err)
	}
	return nil
}

// writeSynced writes the bytes and flushes them to the disk before
// returning. Without the sync the rename can be durable while the content
// behind it is not, which is the very corruption the staging avoids.
func writeSynced(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- path is built from the store root and validated ids
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Remove deletes the record under the given state.
func (s *OutboxStore) Remove(state, id string) error {
	if err := os.Remove(filepath.Join(s.root, state, id+".json")); err != nil {
		return fmt.Errorf("outbox move: %w", err)
	}
	return nil
}

// Find loads a message by id from whichever state directory holds it.
func (s *OutboxStore) Find(id string) (domain.OutboundMessage, error) {
	if err := validOutboxID(id); err != nil {
		return domain.OutboundMessage{}, err
	}
	for _, state := range domain.OutboxStates {
		msg, found, err := s.load(state, id)
		if err != nil {
			return domain.OutboundMessage{}, err
		}
		if found {
			return msg, nil
		}
	}
	return domain.OutboundMessage{}, fmt.Errorf("%w: %s", domain.ErrBadID, id)
}

// FindIn loads the copy held under one specific state. Find cannot express
// that: while a move is half-done the same id exists under two states, and
// recovery has to compare the copies to decide which one is current.
func (s *OutboxStore) FindIn(state, id string) (domain.OutboundMessage, error) {
	if err := validOutboxID(id); err != nil {
		return domain.OutboundMessage{}, err
	}
	msg, found, err := s.load(state, id)
	if err != nil {
		return domain.OutboundMessage{}, err
	}
	if !found {
		return domain.OutboundMessage{}, fmt.Errorf("%w: %s", domain.ErrBadID, id)
	}
	return msg, nil
}

// load reads one state's copy. found separates "no such record here" —
// routine while walking the states — from a record that exists but cannot
// be decoded, which is damage and must never look like absence.
func (s *OutboxStore) load(state, id string) (domain.OutboundMessage, bool, error) {
	// #nosec G304 -- id is validated as filepath.Base by the callers; state is an internal constant
	raw, err := os.ReadFile(filepath.Join(s.root, state, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return domain.OutboundMessage{}, false, nil
	}
	if err != nil {
		return domain.OutboundMessage{}, false, fmt.Errorf("outbox read: %w", err)
	}
	var msg domain.OutboundMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return domain.OutboundMessage{}, false, fmt.Errorf("outbox decode %s/%s: %w", state, id, err)
	}
	return msg, true, nil
}

// validOutboxID keeps an id from addressing anything outside its state dir.
func validOutboxID(id string) error {
	if id == "" || id != filepath.Base(id) {
		return fmt.Errorf("%w: %s", domain.ErrBadID, id)
	}
	return nil
}

// List returns the ids stored under one state.
func (s *OutboxStore) List(state string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(s.root, state, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("outbox list: %w", err)
	}
	ids := make([]string, 0, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		ids = append(ids, base[:len(base)-len(".json")])
	}
	return ids, nil
}

var _ domain.OutboxStore = (*OutboxStore)(nil)
