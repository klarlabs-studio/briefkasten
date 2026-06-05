package application

import (
	"errors"
	"sync"

	"github.com/felixgeelhaar/briefkasten/domain"
)

// Switchable is a Mailbox whose backend can be swapped at runtime
// (runtime reconfiguration). All calls go to the current backend under a
// read lock; Swap replaces it atomically. Optional capabilities forward.
type Switchable struct {
	mu sync.RWMutex
	mb domain.Mailbox
}

// NewSwitchable wraps an initial backend.
func NewSwitchable(mb domain.Mailbox) *Switchable {
	return &Switchable{mb: mb}
}

// Swap replaces the backend for all subsequent calls.
func (s *Switchable) Swap(mb domain.Mailbox) {
	s.mu.Lock()
	s.mb = mb
	s.mu.Unlock()
}

func (s *Switchable) current() domain.Mailbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mb
}

// ListUnread lists the current backend's unread ids.
func (s *Switchable) ListUnread() ([]string, error) { return s.current().ListUnread() }

// Fetch returns the raw message bytes from the current backend.
func (s *Switchable) Fetch(id string) ([]byte, error) { return s.current().Fetch(id) }

// MarkSeen acknowledges a message on the current backend.
func (s *Switchable) MarkSeen(id string) error { return s.current().MarkSeen(id) }

// Search forwards to the backend's Searcher or the generic fallback.
func (s *Switchable) Search(query string) ([]string, error) {
	return searchMailbox(s.current(), query)
}

// Folders forwards to the backend when it supports folders.
func (s *Switchable) Folders() ([]string, error) {
	if fm, ok := s.current().(domain.FolderMailbox); ok {
		return fm.Folders()
	}
	return []string{"INBOX"}, nil
}

// InFolder forwards to the backend when it supports folders.
func (s *Switchable) InFolder(name string) (domain.Mailbox, error) {
	if fm, ok := s.current().(domain.FolderMailbox); ok {
		return fm.InFolder(name)
	}
	if name == "INBOX" {
		return s, nil
	}
	return nil, errors.New("briefkasten: backend has no folder support")
}

// Archive forwards to the backend's Curator.
func (s *Switchable) Archive(id string) error {
	cu, ok := s.current().(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	return cu.Archive(id)
}

// Delete forwards to the backend's Curator.
func (s *Switchable) Delete(id string) error {
	cu, ok := s.current().(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	return cu.Delete(id)
}

var (
	_ domain.Mailbox       = (*Switchable)(nil)
	_ domain.Searcher      = (*Switchable)(nil)
	_ domain.FolderMailbox = (*Switchable)(nil)
	_ domain.Curator       = (*Switchable)(nil)
)
