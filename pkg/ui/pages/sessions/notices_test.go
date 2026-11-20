package sessions

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// TestNoticesFit measures every notice of the tab, with its mark, against
// common.NoticeWidth for a typical session and repository.
func TestNoticesFit(t *testing.T) {
	const label, repo = "infra / claude-1", "me/dotfiles"
	transport := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	fetched := []RepoSync{{Repo: repo, Checkout: openshell.CheckoutUpdate{State: openshell.CheckoutFetched}}}
	missing := []RepoSync{{Repo: repo, Checkout: openshell.CheckoutUpdate{State: openshell.CheckoutMissing}}}
	failed := []RepoSync{{Repo: repo, Checkout: openshell.CheckoutUpdate{State: openshell.CheckoutFailed}}, {Repo: "me/sundae"}}
	cases := map[string]notice{
		"repos failed":      reposFailedNotice(transport),
		"no repos":          noReposNotice(),
		"kept":              keptNotice(label),
		"cancelled":         cancelledNotice(),
		"only repo":         onlyRepoNotice(),
		"not loaded":        notLoadedNotice(),
		"no terminal":       noTerminalNotice(),
		"started":           startedNotice(label),
		"failed":            failedNotice(label, transport),
		"left running":      leftNotice(label, true),
		"left":              leftNotice(label, false),
		"exited":            exitedNotice(label, 127),
		"deleted":           deletedNotice(label),
		"already deleted":   alreadyDeletedNotice(label),
		"suspended":         suspendedNotice(label),
		"already suspended": alreadySuspendedNotice(label),
		"resumed":           resumedNotice(label),
		"added one":         addedNotice(label, []string{repo}),
		"added many":        addedNotice(label, []string{repo, "me/sundae"}),
		"removed one":       removedNotice([]string{repo}),
		"removed many":      removedNotice([]string{repo, "me/sundae"}),
		"synced":            syncNotice(label, nil),
		"synced fetched":    syncNotice(label, fetched),
		"synced missing":    syncNotice(label, missing),
		"sync failed":       syncNotice(label, failed),
	}
	c := common.NewCommon(context.Background(), 200, 24)
	for name, n := range cases {
		if n.text == "" {
			t.Errorf("%s: empty", name)
			continue
		}
		shown := ansi.Strip(c.NoticeView(n.text, n.kind, 200))
		if w := ansi.StringWidth(shown); w > common.NoticeWidth {
			t.Errorf("%s: %q is %d cells, over %d", name, shown, w, common.NoticeWidth)
		}
	}
}
