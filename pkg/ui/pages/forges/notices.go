package forges

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// notice is a one-line message the frame shows in its message slot. Each
// fits common.NoticeWidth with a typical label (notices_test.go).
type notice struct {
	text string
	kind common.MessageKind
}

func info(text string) notice    { return notice{text, common.MessageInfo} }
func success(text string) notice { return notice{text, common.MessageSuccess} }
func failure(text string) notice { return notice{text, common.MessageError} }

func notPushedNotice() notice  { return info("Not pushed.") }
func selectRepoNotice() notice { return info("Select a repository first.") }
func copiedNotice() notice     { return success("Copied the clone command.") }
func allImportedNotice() notice {
	return info("Every repository shown is imported.")
}

func notImportedNotice(path string) notice { return info(path + " is not imported · i import") }

func alreadyImportedNotice(repo string) notice {
	return info("Already imported as " + repo + ".")
}

func importFailedNotice(forgeName string, err error) notice {
	return failure(forgeName + ": import failed: " + common.ShortError(err))
}

func importedNotice(repo string) notice { return success("Imported as " + repo + ".") }

func importedAllNotice(n int, forgeName string) notice {
	return success(fmt.Sprintf("Imported %s from %s.", plural(n, "repository", "repositories"), forgeName))
}

func someFailedNotice(failed, total int, what string) notice {
	return failure(fmt.Sprintf("%d of %d %s failed; see their rows.", failed, total, what))
}

func syncRunningNotice(forgeName string) notice {
	return info("A sync of " + forgeName + " is already running.")
}

func syncFailedNotice(forgeName string, err error) notice {
	return failure(forgeName + ": sync failed: " + common.ShortError(err))
}

func nothingToSyncNotice(forgeName string) notice {
	return info("Listed " + forgeName + "; nothing imported to sync.")
}

func syncedAllNotice(n int, forgeName string) notice {
	return success(fmt.Sprintf("Synced %s from %s.", plural(n, "repository", "repositories"), forgeName))
}

func syncedNotice(res *forge.SyncResult) notice { return success("Synced: " + res.Summary() + ".") }

func noBranchNotice() notice { return failure("No branch to push: HEAD is detached.") }

func cannotPushNotice(err error) notice { return failure("Cannot push: " + common.ShortError(err)) }

// Row outcomes: a few words in the row, and the detail the message slot
// shows while the row is selected.

func importFailed(errText string) result {
	return result{short: "✗ import failed", detail: "Import failed: " + common.ShortErrorText(errText), err: true}
}

func syncFailed(errText string) result {
	return result{short: "✗ sync failed", detail: "Sync failed: " + common.ShortErrorText(errText), err: true}
}

// pushOutcome is the row outcome of a push; a refusal says what to do.
func pushOutcome(err error, res *forge.PushResult, branch, forgeName string) result {
	var ahead *forge.UpstreamAheadError
	switch {
	case errors.As(err, &ahead) && ahead.Stale:
		return result{short: "✗ push refused", detail: "Push refused: " + forgeName + " moved · u sync", err: true}
	case errors.As(err, &ahead):
		return result{short: "✗ push refused", detail: "Push refused: " + forgeName + " is ahead · u sync", err: true}
	case err != nil || res == nil:
		return result{short: "✗ push failed", detail: "Push failed: " + common.ShortError(err), err: true}
	case res.UpToDate:
		return result{short: "✓ up to date", detail: branch + " is already up to date on " + forgeName + "."}
	}
	return result{short: "✓ pushed", detail: fmt.Sprintf("Pushed %s to %s: %s..%s.", branch, forgeName, short(res.Old), short(res.New))}
}

// noticeOf is a row outcome as the notice of the operation.
func noticeOf(r result) notice {
	if r.err {
		return failure(r.detail)
	}
	return success(r.detail)
}

// authFailure matches a forge rejecting its token.
var authFailure = regexp.MustCompile(`\b401\b|(?i:unauthorized|bad credentials|token (?:expired|revoked))`)

// withFix is a forge error for the message slot: a rejected token says
// where to set it instead of the forge's own words.
func withFix(text string) string {
	if authFailure.MatchString(text) {
		return "Token rejected; check forges[].token_file"
	}
	return text
}
