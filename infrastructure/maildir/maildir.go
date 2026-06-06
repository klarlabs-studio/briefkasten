// Package maildir is briefkasten's local-first backend: maildir-style
// directories on disk.
package maildir

import (
	"bytes"
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
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("briefkasten: prepare %s: %w", sub, err)
		}
	}
	return &Mailbox{root: root}, nil
}

// ListUnread returns message ids (filenames) in new/, in stable order.
func (m *Mailbox) ListUnread() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, "new"))
	if err != nil {
		return nil, fmt.Errorf("briefkasten: list: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Fetch returns the raw message bytes for an unread id.
func (m *Mailbox) Fetch(id string) ([]byte, error) {
	path, err := m.safePath("new", id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("briefkasten: fetch %q: %w", id, err)
	}
	return data, nil
}

// MarkSeen moves a message from new/ to cur/.
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
		return fmt.Errorf("briefkasten: mark seen %q: %w", id, err)
	}
	return nil
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
	ids, err := d.ListUnread()
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

var _ domain.Searcher = (*Mailbox)(nil)

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

// moveTo relocates an unread message into a hidden sub-maildir.
func (d *Mailbox) moveTo(sub, id string) error {
	from, err := d.safePath("new", id)
	if err != nil {
		return err
	}
	destDir := filepath.Join(d.root, sub, "new")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("briefkasten: prepare %s: %w", sub, err)
	}
	if err := os.Rename(from, filepath.Join(destDir, id)); err != nil {
		return fmt.Errorf("briefkasten: move %q to %s: %w", id, sub, err)
	}
	return nil
}

// Archive moves an unread message to .archive/new — out of the backlog,
// never destroyed.
func (d *Mailbox) Archive(id string) error { return d.moveTo(".archive", id) }

// Delete moves an unread message to .trash/new — a soft delete; real
// removal stays a human decision outside briefkasten.
func (d *Mailbox) Delete(id string) error { return d.moveTo(".trash", id) }

var _ domain.Curator = (*Mailbox)(nil)
