package maildir

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/felixgeelhaar/briefkasten/domain"
)

// OutboxStore persists outbound messages as JSON files under
// <root>/<state>/<id>.json — a restart resumes exactly where it stopped.
type OutboxStore struct {
	root string
}

// NewOutboxStore creates the state directories.
func NewOutboxStore(root string) (*OutboxStore, error) {
	for _, s := range domain.OutboxStates {
		if err := os.MkdirAll(filepath.Join(root, s), 0o755); err != nil {
			return nil, fmt.Errorf("outbox init: %w", err)
		}
	}
	return &OutboxStore{root: root}, nil
}

// Write persists the message under its current state.
func (s *OutboxStore) Write(msg domain.OutboundMessage) error {
	raw, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return fmt.Errorf("outbox marshal: %w", err)
	}
	path := filepath.Join(s.root, msg.State, msg.ID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("outbox write: %w", err)
	}
	return nil
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
	if id == "" || id != filepath.Base(id) {
		return domain.OutboundMessage{}, fmt.Errorf("%w: %s", domain.ErrBadID, id)
	}
	for _, state := range domain.OutboxStates {
		raw, err := os.ReadFile(filepath.Join(s.root, state, id+".json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return domain.OutboundMessage{}, fmt.Errorf("outbox read: %w", err)
		}
		var msg domain.OutboundMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return domain.OutboundMessage{}, fmt.Errorf("outbox decode: %w", err)
		}
		return msg, nil
	}
	return domain.OutboundMessage{}, fmt.Errorf("%w: %s", domain.ErrBadID, id)
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
