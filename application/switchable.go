package application

import (
	"context"
	"errors"
	"sync"

	"go.klarlabs.de/briefkasten/domain"
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
func (s *Switchable) ListUnread(ctx context.Context) ([]string, error) {
	return s.current().ListUnread(ctx)
}

// List forwards to the backend's ScopedMailbox when it has one.
func (s *Switchable) List(ctx context.Context, scope domain.Scope) ([]string, error) {
	return listMailbox(ctx, s.current(), scope)
}

// Fetch returns the raw message bytes from the current backend.
func (s *Switchable) Fetch(ctx context.Context, id string) ([]byte, error) {
	return s.current().Fetch(ctx, id)
}

// MarkSeen acknowledges a message on the current backend.
func (s *Switchable) MarkSeen(ctx context.Context, id string) error {
	return s.current().MarkSeen(ctx, id)
}

// MarkSeenMany forwards to the backend's BulkMailbox, or loops.
func (s *Switchable) MarkSeenMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return markSeenMany(ctx, s.current(), ids)
}

// Search forwards to the backend's Searcher or the generic fallback.
func (s *Switchable) Search(ctx context.Context, query string) ([]string, error) {
	return searchMailbox(ctx, s.current(), domain.ScopeUnread, query)
}

// SearchScope forwards to the backend's ScopedSearcher or the fallback.
func (s *Switchable) SearchScope(ctx context.Context, scope domain.Scope, query string) ([]string, error) {
	return searchMailbox(ctx, s.current(), scope, query)
}

// Folders forwards to the backend when it supports folders.
func (s *Switchable) Folders(ctx context.Context) ([]string, error) {
	if fm, ok := s.current().(domain.FolderMailbox); ok {
		return fm.Folders(ctx)
	}
	return []string{"INBOX"}, nil
}

// InFolder forwards to the backend when it supports folders.
func (s *Switchable) InFolder(ctx context.Context, name string) (domain.Mailbox, error) {
	if fm, ok := s.current().(domain.FolderMailbox); ok {
		return fm.InFolder(ctx, name)
	}
	if name == "INBOX" {
		return s, nil
	}
	return nil, errors.New("briefkasten: backend has no folder support")
}

// Archive forwards to the backend's Curator.
func (s *Switchable) Archive(ctx context.Context, id string) error {
	cu, ok := s.current().(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	return cu.Archive(ctx, id)
}

// Delete forwards to the backend's Curator.
func (s *Switchable) Delete(ctx context.Context, id string) error {
	cu, ok := s.current().(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	return cu.Delete(ctx, id)
}

// ArchiveMany forwards to the backend's BulkCurator, or loops over its
// Curator. Without the forward the capability would vanish behind this
// decorator and every batch would silently fall back to one call per id.
func (s *Switchable) ArchiveMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return curateMany(ctx, s.current(), ids, actionArchive)
}

// DeleteMany forwards to the backend's BulkCurator, or loops over its
// Curator.
func (s *Switchable) DeleteMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return curateMany(ctx, s.current(), ids, actionDelete)
}

// CurationPlan forwards to the backend's inspector. The destinations
// belong to whichever backend is current, so a runtime swap changes the
// answer — which is exactly why it is asked rather than remembered.
func (s *Switchable) CurationPlan(ctx context.Context) (domain.CurationPlan, error) {
	ci, ok := s.current().(domain.CurationInspector)
	if !ok {
		return domain.CurationPlan{}, errors.New("briefkasten: backend cannot report curation destinations")
	}
	return ci.CurationPlan(ctx)
}

var (
	_ domain.Mailbox           = (*Switchable)(nil)
	_ domain.ScopedMailbox     = (*Switchable)(nil)
	_ domain.Searcher          = (*Switchable)(nil)
	_ domain.ScopedSearcher    = (*Switchable)(nil)
	_ domain.FolderMailbox     = (*Switchable)(nil)
	_ domain.Curator           = (*Switchable)(nil)
	_ domain.CurationInspector = (*Switchable)(nil)
	_ domain.BulkMailbox       = (*Switchable)(nil)
	_ domain.BulkCurator       = (*Switchable)(nil)
)
