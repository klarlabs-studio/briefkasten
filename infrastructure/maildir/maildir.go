// Package maildir is briefkasten's local-first backend: maildir-style
// directories on disk.
package maildir

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/briefkasten/domain"
)

// Mailbox is the local-first backend: a maildir-style directory where
// new/ holds unread .eml files and cur/ holds seen ones. Dropping a file
// into new/ is "receiving mail" — ideal for development, testing, and
// pipelines that already export messages to disk.
type Mailbox struct {
	root string
}

// New prepares the directory layout (root/new, root/cur).
func New(root string) (*Mailbox, error) {
	for _, sub := range []string{"new", "cur"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			return nil, fmt.Errorf("briefkasten: prepare %s: %w", sub, err)
		}
	}
	return &Mailbox{root: root}, nil
}

// ListUnread returns message ids (filenames) in new/, in stable order.
func (m *Mailbox) ListUnread() ([]string, error) { return m.List(domain.ScopeUnread) }

// List returns message ids for the scope, in stable order: new/ for
// unread, cur/ for read, and the union of both for all.
func (m *Mailbox) List(scope domain.Scope) ([]string, error) {
	var subs []string
	switch scope {
	case domain.ScopeUnread:
		subs = []string{"new"}
	case domain.ScopeRead:
		subs = []string{"cur"}
	case domain.ScopeAll:
		subs = []string{"new", "cur"}
	default:
		return nil, fmt.Errorf("%w: %q", domain.ErrBadScope, string(scope))
	}

	seen := make(map[string]struct{})
	ids := []string{}
	for _, sub := range subs {
		entries, err := os.ReadDir(filepath.Join(m.root, sub))
		if err != nil {
			return nil, fmt.Errorf("briefkasten: list: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			// A message that was marked seen mid-listing can appear in
			// both dirs; report it once.
			if _, dup := seen[e.Name()]; dup {
				continue
			}
			seen[e.Name()] = struct{}{}
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

var _ domain.ScopedMailbox = (*Mailbox)(nil)

// Fetch returns the raw message bytes for an id, unread (new/) or
// already read (cur/). Reading never moves the message, so fetching a
// seen message leaves it seen and an unread one unread.
func (m *Mailbox) Fetch(id string) ([]byte, error) {
	var firstErr error
	for _, sub := range []string{"new", "cur"} {
		path, err := m.safePath(sub, id)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path) // #nosec G304 -- path built by safePath, which rejects ids that escape the mailbox
		if err == nil {
			return data, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("briefkasten: fetch %q: %w", id, firstErr)
}

// MarkSeen moves a message from new/ to cur/. Acknowledging a message
// that is already read succeeds without doing anything — the tool is
// idempotent, and an agent re-acknowledging its work is not an error.
// An id in neither directory is a bad id, not a backend fault.
func (m *Mailbox) MarkSeen(id string) error {
	from, err := m.safePath("new", id)
	if err != nil {
		return err
	}
	to, err := m.safePath("cur", id)
	if err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("briefkasten: mark seen %q: %w", id, err)
		}
		if _, statErr := os.Stat(to); statErr == nil {
			return nil
		}
		return fmt.Errorf("%w: %q", domain.ErrBadID, id)
	}
	return nil
}

// locate resolves an id to its on-disk path, preferring the unread
// backlog (new/) and falling back to read mail (cur/).
func (m *Mailbox) locate(id string) (string, error) {
	newPath, err := m.safePath("new", id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(newPath); err == nil {
		return newPath, nil
	}
	curPath, err := m.safePath("cur", id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(curPath); err == nil {
		return curPath, nil
	}
	return "", fmt.Errorf("%w: %q", domain.ErrBadID, id)
}

// safePath joins root/sub/id, rejecting ids that escape the mailbox.
func (m *Mailbox) safePath(sub, id string) (string, error) {
	if id == "" || id != filepath.Base(id) || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("%w: %q", domain.ErrBadID, id)
	}
	return filepath.Join(m.root, sub, id), nil
}

// Search scans the unread backlog for a case-insensitive substring match.
func (d *Mailbox) Search(query string) ([]string, error) {
	return d.SearchScope(domain.ScopeUnread, query)
}

// SearchScope scans the scope's messages for a case-insensitive
// substring match.
func (d *Mailbox) SearchScope(scope domain.Scope, query string) ([]string, error) {
	ids, err := d.List(scope)
	if err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		raw, err := d.Fetch(id)
		if err != nil {
			continue
		}
		if bytes.Contains(bytes.ToLower(raw), needle) {
			out = append(out, id)
		}
	}
	return out, nil
}

var (
	_ domain.Searcher       = (*Mailbox)(nil)
	_ domain.ScopedSearcher = (*Mailbox)(nil)
)

// Folders lists the root maildir ("INBOX") plus every subdirectory that
// looks like a maildir (contains new/).
func (d *Mailbox) Folders() ([]string, error) {
	folders := []string{"INBOX"}
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, fmt.Errorf("briefkasten: folders: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "new" || e.Name() == "cur" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if st, err := os.Stat(filepath.Join(d.root, e.Name(), "new")); err == nil && st.IsDir() {
			folders = append(folders, e.Name())
		}
	}
	sort.Strings(folders[1:])
	return folders, nil
}

// InFolder returns a Mailbox over the named sub-maildir; "INBOX" is the
// root. Folder names cannot escape the root.
func (d *Mailbox) InFolder(name string) (domain.Mailbox, error) {
	if name == "INBOX" {
		return d, nil
	}
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("briefkasten: invalid folder %q", name)
	}
	return New(filepath.Join(d.root, name))
}

var _ domain.FolderMailbox = (*Mailbox)(nil)

// moveTo relocates a message into a hidden sub-maildir. Read messages
// (cur/) curate just like unread ones (new/).
func (d *Mailbox) moveTo(sub, id string) error {
	from, err := d.locate(id)
	if err != nil {
		return err
	}
	destDir := filepath.Join(d.root, sub, "new")
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("briefkasten: prepare %s: %w", sub, err)
	}
	if err := os.Rename(from, filepath.Join(destDir, id)); err != nil {
		return fmt.Errorf("briefkasten: move %q to %s: %w", id, sub, err)
	}
	return nil
}

// Archive moves a message — read or unread — to .archive/new: out of
// the backlog, never destroyed.
func (d *Mailbox) Archive(id string) error { return d.moveTo(".archive", id) }

// Delete moves a message — read or unread — to .trash/new: a soft
// delete; real removal stays a human decision outside briefkasten.
func (d *Mailbox) Delete(id string) error { return d.moveTo(".trash", id) }

// CurationPlan reports where curation files messages. The dir backend
// owns its whole layout, so there is nothing to discover — the answer is
// the same every time, and reported only so the surface matches the IMAP
// backend rather than leaving humans to guess which one they are on.
func (d *Mailbox) CurationPlan() (domain.CurationPlan, error) {
	return domain.CurationPlan{
		Archive: domain.CurationDestination{Folder: ".archive", Route: domain.RouteFixed},
		Trash:   domain.CurationDestination{Folder: ".trash", Route: domain.RouteFixed},
	}, nil
}

var (
	_ domain.Curator           = (*Mailbox)(nil)
	_ domain.CurationInspector = (*Mailbox)(nil)
)
