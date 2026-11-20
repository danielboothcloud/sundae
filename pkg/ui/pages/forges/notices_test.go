package forges

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// TestNoticesFit measures every notice and row detail of the tab, with its
// mark, against common.NoticeWidth for a typical forge and repository.
func TestNoticesFit(t *testing.T) {
	const forgeName, path, repo, branch = "github", "me/dotfiles", "github/me/dotfiles", "main"
	transport := errors.New(`Get "https://api.github.com/user/repos": dial tcp 10.0.0.1:443: connect: connection refused`)
	synced := &forge.SyncResult{Branches: []forge.BranchStatus{{Name: "main", Action: forge.BranchFastForwarded}}}
	cases := map[string]notice{
		"not pushed":       notPushedNotice(),
		"select repo":      selectRepoNotice(),
		"copied":           copiedNotice(),
		"all imported":     allImportedNotice(),
		"not imported":     notImportedNotice(path),
		"already imported": alreadyImportedNotice(repo),
		"import failed":    importFailedNotice(forgeName, transport),
		"imported":         importedNotice(repo),
		"imported all":     importedAllNotice(12, forgeName),
		"imports failed":   someFailedNotice(2, 12, "imports"),
		"syncs failed":     someFailedNotice(2, 12, "syncs"),
		"sync running":     syncRunningNotice(forgeName),
		"sync failed":      syncFailedNotice(forgeName, transport),
		"nothing to sync":  nothingToSyncNotice(forgeName),
		"synced all":       syncedAllNotice(12, forgeName),
		"synced":           syncedNotice(synced),
		"no branch":        noBranchNotice(),
		"cannot push":      cannotPushNotice(transport),
		"row import":       noticeOf(importFailed(transport.Error())),
		"row sync":         noticeOf(syncFailed(transport.Error())),
		"push refused":     noticeOf(pushOutcome(&forge.UpstreamAheadError{Branch: branch, Missing: 2}, nil, branch, forgeName)),
		"push stale":       noticeOf(pushOutcome(&forge.UpstreamAheadError{Branch: branch, Stale: true}, nil, branch, forgeName)),
		"push failed":      noticeOf(pushOutcome(transport, nil, branch, forgeName)),
		"up to date":       noticeOf(pushOutcome(nil, &forge.PushResult{UpToDate: true}, branch, forgeName)),
		"pushed":           noticeOf(pushOutcome(nil, &forge.PushResult{Old: "1111111aaa", New: "2222222bbb"}, branch, forgeName)),
		"token":            failure(withFix(forgeName + ": cannot list: 401 Unauthorized")),
		"header error":     failure(withFix(forgeName + ": cannot list: " + tidyError(forgeName, transport.Error()))),
		"last sync":        failure(withFix("Last sync failed: " + common.ShortError(transport))),
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
