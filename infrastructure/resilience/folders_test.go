package resilience

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/domain"

	"go.klarlabs.de/fortify/ferrors"
)

// managingMailbox adds the domain.FolderManager capability, counting
// calls so a test can tell one attempt from a retried one.
type managingMailbox struct {
	stubMailbox
	created   []string
	deleted   []string
	deleteErr error
}

func (m *managingMailbox) CreateFolder(_ context.Context, name string) error {
	m.created = append(m.created, name)
	return nil
}

func (m *managingMailbox) DeleteFolder(_ context.Context, name string) error {
	m.deleted = append(m.deleted, name)
	return m.deleteErr
}

// The capability has to survive the decorator. This fails if the
// forward is dropped: the wrapper would answer "cannot create or delete
// folders" for a backend that plainly can.
func TestResilientForwardsFolderManagement(t *testing.T) {
	mb := &managingMailbox{}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	if err := r.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := r.DeleteFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if len(mb.created) != 1 || mb.created[0] != "Work" {
		t.Errorf("created = %v, want the call to have reached the backend", mb.created)
	}
	if len(mb.deleted) != 1 || mb.deleted[0] != "Work" {
		t.Errorf("deleted = %v, want the call to have reached the backend", mb.deleted)
	}
}

func TestResilientFolderManagementWithoutSupport(t *testing.T) {
	r := Wrap(&stubMailbox{}, Config{})

	for _, call := range []func() error{
		func() error { return r.CreateFolder(t.Context(), "Work") },
		func() error { return r.DeleteFolder(t.Context(), "Work") },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "cannot create or delete folders") {
			t.Errorf("err = %v, want the missing-capability answer", err)
		}
	}
}

// A refused delete is the caller's request being wrong, not a backend
// fault: retrying it would only re-count the same folder, and a caller
// who keeps asking must not open the breaker on everyone else's behalf.
func TestResilientFolderRefusalsNotRetriedOrCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"not empty", fmt.Errorf("%w: %q holds 3 messages", domain.ErrFolderNotEmpty, "Work")},
		{"protected", fmt.Errorf("%w: %q is the mailbox itself", domain.ErrFolderProtected, "INBOX")},
		{"bad name", fmt.Errorf("%w: no folder named %q", domain.ErrBadFolder, "Ghost")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mb := &managingMailbox{deleteErr: tc.err}
			r := Wrap(mb, Config{InitialDelay: time.Millisecond})

			// Well past the breaker's five-failure threshold.
			for i := 0; i < 8; i++ {
				err := r.DeleteFolder(t.Context(), "Work")
				if !errors.Is(err, tc.err) {
					t.Fatalf("attempt %d: err = %v, want the refusal", i, err)
				}
				if errors.Is(err, ferrors.ErrCircuitOpen) {
					t.Fatalf("attempt %d opened the breaker on a caller mistake", i)
				}
			}
			if len(mb.deleted) != 8 {
				t.Errorf("backend calls = %d, want 8 — one attempt per call, no retries", len(mb.deleted))
			}
		})
	}
}

var _ domain.FolderManager = (*Mailbox)(nil)
