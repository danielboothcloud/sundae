package selection

import (
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// notice is a one-line message for the frame's message slot. Each fits
// common.NoticeWidth with a typical label (notices_test.go).
type notice struct {
	text string
	kind common.MessageKind
}

func unauthorizedNotice(repo string) notice {
	return notice{repo + ": " + proto.ErrUnauthorized.Error(), common.MessageError}
}

func keptNotice(repo string) notice     { return notice{"Kept " + repo + ".", common.MessageInfo} }
func deletingNotice(repo string) notice { return notice{"Deleting " + repo, common.MessageBusy} }
func deletedNotice(repo string) notice  { return notice{"Deleted " + repo + ".", common.MessageSuccess} }
func creatingNotice(repo string) notice { return notice{"Creating " + repo, common.MessageBusy} }
func createdNotice(repo string) notice  { return notice{"Created " + repo + ".", common.MessageSuccess} }

// failedNotice is what went wrong with repo, shortened; the caller logs
// the whole error.
func failedNotice(repo string, err error) notice {
	return notice{repo + ": " + common.ShortError(err), common.MessageError}
}
