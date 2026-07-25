package maildir

import (
	"strings"
	"testing"
)

// Sizes is what bounds a batched fetch on this backend, so it has to be
// the file's real size, and it has to answer for read mail as well as
// unread — an id carries no read state, and a fetch that can reach a
// message must be measurable before it does.
func TestDirMailboxSizes(t *testing.T) {
	mb, root := newDir(t)
	unread := "From: a@b.c\r\nSubject: A\r\n\r\n" + strings.Repeat("x", 100)
	read := "From: a@b.c\r\nSubject: B\r\n\r\n" + strings.Repeat("y", 200)
	drop(t, root, "a.eml", unread)
	drop(t, root, "b.eml", read)
	if err := mb.MarkSeen(t.Context(), "b.eml"); err != nil {
		t.Fatal(err)
	}

	sizes, err := mb.Sizes(t.Context(), []string{"a.eml", "b.eml"})
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	if sizes["a.eml"] != int64(len(unread)) || sizes["b.eml"] != int64(len(read)) {
		t.Errorf("sizes = %v, want %d and %d", sizes, len(unread), len(read))
	}
}

// An id the mailbox does not hold — unknown, or one trying to escape it
// — is left out of the measurement rather than failing the call. It
// becomes its own failure when the fetch runs, and the ids that are here
// still get measured.
func TestDirMailboxSizesOmitsUnknownIDs(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: a@b.c\r\n\r\nhi")

	sizes, err := mb.Sizes(t.Context(), []string{"a.eml", "ghost.eml", "../escape.eml", ""})
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	if len(sizes) != 1 {
		t.Fatalf("sizes = %v, want only the message that is here", sizes)
	}
	if _, measured := sizes["a.eml"]; !measured {
		t.Errorf("sizes = %v, want a.eml measured", sizes)
	}
}
