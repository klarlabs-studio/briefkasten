package application_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// managedBox adds the domain.FolderManager capability to memBox, and
// records what reached it — the only way to tell a forwarded call from
// one a decorator answered on the backend's behalf.
type managedBox struct {
	*memBox
	created []string
	deleted []string
}

func newManagedBox() *managedBox { return &managedBox{memBox: newMemBox(map[string]string{})} }

func (m *managedBox) CreateFolder(_ context.Context, name string) error {
	m.created = append(m.created, name)
	m.folders[name] = newMemBox(map[string]string{})
	return nil
}

func (m *managedBox) DeleteFolder(_ context.Context, name string) error {
	if _, ok := m.folders[name]; !ok {
		return domain.ErrBadFolder
	}
	m.deleted = append(m.deleted, name)
	delete(m.folders, name)
	return nil
}

func TestServiceCreatesAndDeletesFolders(t *testing.T) {
	box := newManagedBox()
	other := newManagedBox()
	svc := application.NewService(box, map[string]domain.Mailbox{"business": other})

	if err := svc.CreateFolder(t.Context(), "", "Work"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folders, err := svc.Folders(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(folders, "Work") {
		t.Errorf("folders = %v, want Work", folders)
	}

	// The account routes the call, so a folder is created where it was
	// asked for and nowhere else.
	if err := svc.CreateFolder(t.Context(), "business", "Kunden"); err != nil {
		t.Fatalf("CreateFolder(business): %v", err)
	}
	if !slices.Equal(box.created, []string{"Work"}) || !slices.Equal(other.created, []string{"Kunden"}) {
		t.Errorf("created: default %v, business %v", box.created, other.created)
	}

	if err := svc.DeleteFolder(t.Context(), "", "Work"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if !slices.Equal(box.deleted, []string{"Work"}) {
		t.Errorf("deleted = %v, want [Work]", box.deleted)
	}
	if err := svc.CreateFolder(t.Context(), "nope", "Work"); err == nil {
		t.Error("unknown account accepted")
	}
}

// A backend that can only list folders says so rather than reporting a
// folder it never made.
func TestServiceFolderManagementWithoutSupport(t *testing.T) {
	svc := application.NewService(bareBox{newMemBox(map[string]string{})}, nil)

	for _, call := range []func() error{
		func() error { return svc.CreateFolder(t.Context(), "", "Work") },
		func() error { return svc.DeleteFolder(t.Context(), "", "Work") },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "cannot create or delete folders") {
			t.Errorf("err = %v, want the missing-capability answer", err)
		}
	}
}

// Every optional capability has to be forwarded through Switchable or it
// vanishes behind the decorator. This test fails if the forward is
// dropped.
func TestSwitchableForwardsFolderManagement(t *testing.T) {
	box := newManagedBox()
	sw := application.NewSwitchable(box)

	if err := sw.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if !slices.Equal(box.created, []string{"Work"}) {
		t.Fatalf("created = %v, want the call to have reached the backend", box.created)
	}
	if err := sw.DeleteFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if !slices.Equal(box.deleted, []string{"Work"}) {
		t.Fatalf("deleted = %v, want the call to have reached the backend", box.deleted)
	}
	if err := sw.DeleteFolder(t.Context(), "Ghost"); !errors.Is(err, domain.ErrBadFolder) {
		t.Errorf("err = %v, want the backend's own refusal to travel out", err)
	}

	// After a swap the new backend answers — the folders belong to
	// whichever mailbox is current.
	next := newManagedBox()
	sw.Swap(next)
	if err := sw.CreateFolder(t.Context(), "Later"); err != nil {
		t.Fatalf("CreateFolder after swap: %v", err)
	}
	if !slices.Equal(next.created, []string{"Later"}) || len(box.created) != 1 {
		t.Errorf("after swap: next %v, previous %v", next.created, box.created)
	}

	// Swapped onto a backend without the capability, it says so.
	sw.Swap(bareBox{newMemBox(map[string]string{})})
	if err := sw.CreateFolder(t.Context(), "Nope"); err == nil ||
		!strings.Contains(err.Error(), "cannot create or delete folders") {
		t.Errorf("err = %v, want the missing-capability answer", err)
	}
}

var _ domain.FolderManager = (*application.Switchable)(nil)
