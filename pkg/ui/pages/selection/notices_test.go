package selection

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// TestNoticesFit measures every notice of the Repositories tab, with its
// mark, against common.NoticeWidth for a typical repository.
func TestNoticesFit(t *testing.T) {
	const repo = "me/dotfiles"
	cases := map[string]notice{
		"unauthorized": unauthorizedNotice(repo),
		"kept":         keptNotice(repo),
		"deleting":     deletingNotice(repo),
		"deleted":      deletedNotice(repo),
		"creating":     creatingNotice(repo),
		"created":      createdNotice(repo),
		"failed":       failedNotice(repo, errors.New("dial tcp 10.0.0.1:443: connect: connection refused")),
	}
	c := common.NewCommon(context.Background(), 200, 24)
	for name, n := range cases {
		shown := ansi.Strip(c.NoticeView(n.text, n.kind, 200))
		if w := ansi.StringWidth(shown); n.text == "" || w > common.NoticeWidth {
			t.Errorf("%s: %q is %d cells, over %d", name, shown, w, common.NoticeWidth)
		}
	}
}
