// Package application holds briefkasten's use cases — the single code
// path shared by every interface: the MCP tools, the CLI, and any future
// surface all call these methods. Confirmation of destructive operations
// is an interface concern (MCP elicitation, CLI prompt); the use cases
// here execute after approval.
package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.klarlabs.de/briefkasten/domain"
)

// Service exposes the mailbox use cases over a default mailbox and any
// named accounts.
type Service struct {
	mailbox  domain.Mailbox
	accounts map[string]domain.Mailbox
}

// NewService wires the use cases.
func NewService(mailbox domain.Mailbox, accounts map[string]domain.Mailbox) *Service {
	return &Service{mailbox: mailbox, accounts: accounts}
}

// Resolve routes an optional account and folder to the target mailbox.
func (s *Service) Resolve(ctx context.Context, account, folder string) (domain.Mailbox, error) {
	box := s.mailbox
	if account != "" {
		named, ok := s.accounts[account]
		if !ok {
			return nil, fmt.Errorf("briefkasten: unknown account %q", account)
		}
		box = named
	}
	if folder == "" {
		return box, nil
	}
	fm, ok := box.(domain.FolderMailbox)
	if !ok {
		return nil, errors.New("briefkasten: backend has no folder support")
	}
	return fm.InFolder(ctx, folder)
}

// ListUnread returns the unread ids of the resolved mailbox.
func (s *Service) ListUnread(ctx context.Context, account, folder string) ([]string, error) {
	return s.List(ctx, account, folder, domain.ScopeUnread)
}

// List returns the ids of the resolved mailbox within scope: the unread
// backlog, the already-read mail, or both. An empty scope means unread,
// so callers that predate scopes are unaffected.
func (s *Service) List(ctx context.Context, account, folder string, scope domain.Scope) ([]string, error) {
	scope, err := domain.ParseScope(string(scope))
	if err != nil {
		return nil, err
	}
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	ids, err := listMailbox(ctx, box, scope)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// Read returns the raw message bytes.
func (s *Service) Read(ctx context.Context, account, folder, id string) ([]byte, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	return box.Fetch(ctx, id)
}

// MarkSeen acknowledges a processed message. Idempotent: a message that
// is already read stays read and the call succeeds.
func (s *Service) MarkSeen(ctx context.Context, account, folder, id string) error {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return err
	}
	return box.MarkSeen(ctx, id)
}

// Search finds unread messages matching the query. Backends with a
// Searcher search natively; everything else gets the scan fallback.
func (s *Service) Search(ctx context.Context, account, folder, query string) ([]string, error) {
	return s.SearchScope(ctx, account, folder, query, domain.ScopeUnread)
}

// SearchScope finds messages within scope matching the query.
func (s *Service) SearchScope(ctx context.Context, account, folder, query string, scope domain.Scope) ([]string, error) {
	scope, err := domain.ParseScope(string(scope))
	if err != nil {
		return nil, err
	}
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	ids, err := searchMailbox(ctx, box, scope, query)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// Folders lists the resolved account's folders.
func (s *Service) Folders(ctx context.Context, account string) ([]string, error) {
	box, err := s.Resolve(ctx, account, "")
	if err != nil {
		return nil, err
	}
	if fm, ok := box.(domain.FolderMailbox); ok {
		return fm.Folders(ctx)
	}
	return []string{"INBOX"}, nil
}

// Accounts returns the configured account names; "default" is first.
func (s *Service) Accounts() []string {
	names := []string{"default"}
	for name := range s.accounts {
		names = append(names, name)
	}
	sort.Strings(names[1:])
	return names
}

// Archive files a message away — soft, never destroyed. Read and unread
// messages curate alike; ids come from any scope of List or SearchScope.
// The caller must have obtained human confirmation.
func (s *Service) Archive(ctx context.Context, account, folder, id string) error {
	cu, err := s.curator(ctx, account, folder)
	if err != nil {
		return err
	}
	return cu.Archive(ctx, id)
}

// Delete moves a message to trash — soft delete, never expunged. Read
// and unread messages curate alike. The caller must have obtained human
// confirmation.
func (s *Service) Delete(ctx context.Context, account, folder, id string) error {
	cu, err := s.curator(ctx, account, folder)
	if err != nil {
		return err
	}
	return cu.Delete(ctx, id)
}

// CurationPlan reports where Archive and Delete would file, without
// moving anything — so a human can check the destination before
// approving a move rather than inferring it from where mail landed.
func (s *Service) CurationPlan(ctx context.Context, account, folder string) (domain.CurationPlan, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return domain.CurationPlan{}, err
	}
	ci, ok := box.(domain.CurationInspector)
	if !ok {
		return domain.CurationPlan{}, errors.New("briefkasten: backend cannot report curation destinations")
	}
	return ci.CurationPlan(ctx)
}

func (s *Service) curator(ctx context.Context, account, folder string) (domain.Curator, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	cu, ok := box.(domain.Curator)
	if !ok {
		return nil, errors.New("briefkasten: backend has no curation support")
	}
	return cu, nil
}

// listMailbox lists via the backend's ScopedMailbox when available. A
// backend without it can only speak for the unread backlog, so a wider
// scope fails loudly instead of quietly returning unread mail.
func listMailbox(ctx context.Context, mb domain.Mailbox, scope domain.Scope) ([]string, error) {
	if sm, ok := mb.(domain.ScopedMailbox); ok {
		return sm.List(ctx, scope)
	}
	if scope != domain.ScopeUnread {
		return nil, fmt.Errorf("briefkasten: backend lists unread mail only, scope %q unsupported", string(scope))
	}
	return mb.ListUnread(ctx)
}

// searchMailbox searches via the backend's ScopedSearcher or Searcher
// when available, otherwise falls back to scanning the scope's ids.
func searchMailbox(ctx context.Context, mb domain.Mailbox, scope domain.Scope, query string) ([]string, error) {
	if s, ok := mb.(domain.ScopedSearcher); ok {
		return s.SearchScope(ctx, scope, query)
	}
	if s, ok := mb.(domain.Searcher); ok && scope == domain.ScopeUnread {
		return s.Search(ctx, query)
	}
	ids, err := listMailbox(ctx, mb, scope)
	if err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		raw, err := mb.Fetch(ctx, id)
		if err != nil {
			// Skip what cannot be read, but stop once the caller's time
			// is up rather than walking the rest of the backlog to
			// collect the same error id by id.
			if ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		if bytes.Contains(bytes.ToLower(raw), needle) {
			out = append(out, id)
		}
	}
	return out, nil
}
