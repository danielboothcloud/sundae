package repo

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// TestRepoCopyMessage checks what was copied goes to the frame's message
// slot, not the status bar, until the next key.
func TestRepoCopyMessage(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	r := New(c, NewReadme(c))
	r.Update(CopyMsg{Text: "x", Message: "Command copied to clipboard"})
	if text, kind := r.Message(); text != "Command copied to clipboard" || kind != common.MessageInfo {
		t.Fatalf("message %q %v", text, kind)
	}
	r.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if text, _ := r.Message(); text != "" {
		t.Fatalf("message kept after a key: %q", text)
	}
}
