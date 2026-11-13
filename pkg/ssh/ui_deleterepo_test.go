package ssh

import (
	"context"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/proto"
)

// selectRepo moves the Repositories tab's cursor to name.
func selectRepo(t *testing.T, ui *UI, name string) {
	t.Helper()
	for range 10 {
		if selected(ui) == name {
			return
		}
		uiKey(ui, "j")
	}
	t.Fatalf("%s not in the list (at %q)", name, selected(ui))
}

func TestDeleteRepoConfirmsAndDeletes(t *testing.T) {
	ui, _, be, _ := newRepoUI(t, 80, true)
	ctx := context.Background()
	alice, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "mine", alice, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	ui.Init()
	uiKey(ui, "1")
	runUI(ui, ui.pages[selectionPage].Init(), 2)
	selectRepo(t, ui, "mine")

	// Any key but y cancels.
	uiKey(ui, "D")
	waitScreen(t, ui, "Delete mine?")
	uiKey(ui, "n")
	if _, err := be.Repository(ctx, "mine"); err != nil {
		t.Fatalf("deleted after no: %v", err)
	}

	uiKey(ui, "D")
	uiKey(ui, "y")
	waitScreen(t, ui, "Deleted mine.")
	if _, err := be.Repository(ctx, "mine"); err == nil {
		t.Fatal("repository still exists")
	}
	if s := screenText(ui); strings.Contains(s, "┃ mine") {
		t.Fatalf("still listed:\n%s", s)
	}
}

func TestDeleteRepoNeedsWriteAccess(t *testing.T) {
	ui, _, be, _ := newRepoUI(t, 80, true)
	ctx := context.Background()
	runUI(ui, ui.pages[selectionPage].Init(), 2)
	selectRepo(t, ui, "scratch") // the admin's; alice may only read it
	for _, b := range ui.ShortHelp() {
		if b.Help().Key == "D" {
			t.Fatal("D offered without write access")
		}
	}
	uiKey(ui, "D")
	waitScreen(t, ui, "✗ scratch: unauthorized")
	if _, err := be.Repository(ctx, "scratch"); err != nil {
		t.Fatal(err)
	}
}
