package repo

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// TestCopyNoticesFit measures what the page says it copied, with its mark,
// against common.NoticeWidth.
func TestCopyNoticesFit(t *testing.T) {
	c := common.NewCommon(context.Background(), 200, 24)
	for _, text := range []string{copiedCloneCmd, copiedRef, copiedDiff, copiedHash, copiedContents, copiedFileName, copiedStashMessage, copiedStashPatch} {
		shown := ansi.Strip(c.NoticeView(text, common.MessageSuccess, 200))
		if w := ansi.StringWidth(shown); w > common.NoticeWidth {
			t.Errorf("%q is %d cells, over %d", shown, w, common.NoticeWidth)
		}
	}
}

// TestRepoCopyMessage checks what was copied goes to the frame's message
// slot until the next key.
func TestRepoCopyMessage(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	r := New(c, NewReadme(c))
	r.Update(CopyMsg{Text: "x", Message: copiedCloneCmd})
	if text, kind := r.Message(); text != copiedCloneCmd || kind != common.MessageSuccess {
		t.Fatalf("message %q %v", text, kind)
	}
	r.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if text, _ := r.Message(); text != "" {
		t.Fatalf("message kept after a key: %q", text)
	}
}
