package sessions

import (
	"fmt"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/openshell"
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

func reposFailedNotice(err error) notice {
	return failure("Cannot list repos: " + common.ShortError(err))
}

func noReposNotice() notice             { return info("No repository to start a session for.") }
func keptNotice(label string) notice    { return info("Kept " + label + ".") }
func cancelledNotice() notice           { return info("Cancelled.") }
func onlyRepoNotice() notice            { return info("Cannot remove its only repository.") }
func notLoadedNotice() notice           { return failure("Sessions are not loaded yet · r retry") }
func noTerminalNotice() notice          { return failure("Needs ssh -t to attach · c copy command") }
func startedNotice(label string) notice { return success("Started " + label + ".") }

// failedNotice is what went wrong with a session, shortened; the caller
// logs the whole error.
func failedNotice(label string, err error) notice {
	return failure(label + ": " + common.ShortError(err))
}

// leftNotice is the user leaving an attached session; running reports
// that the program still runs.
func leftNotice(label string, running bool) notice {
	if running {
		return info("Left " + label + "; it keeps running.")
	}
	return info("Left " + label + ".")
}

func exitedNotice(label string, code int) notice {
	return info(fmt.Sprintf("%s: exit status %d.", label, code))
}

func deletedNotice(label string) notice          { return success("Deleted " + label + ".") }
func alreadyDeletedNotice(label string) notice   { return info(label + " was already deleted.") }
func suspendedNotice(label string) notice        { return success("Suspended " + label + "; storage kept.") }
func alreadySuspendedNotice(label string) notice { return info(label + " is already suspended.") }
func resumedNotice(label string) notice          { return success("Resumed " + label + ".") }

func addedNotice(label string, repos []string) notice {
	if len(repos) == 1 {
		return success("Added " + repos[0] + " to " + label + ".")
	}
	return success(fmt.Sprintf("Added %d repos to %s.", len(repos), label))
}

// removedNotice says the checkouts stay: removing a repository from a
// session does not delete its work.
func removedNotice(repos []string) notice {
	if len(repos) == 1 {
		return success("Removed " + repos[0] + "; its checkout stays.")
	}
	return success(fmt.Sprintf("Removed %d repos; their checkouts stay.", len(repos)))
}

// syncFailed reports whether the sync of a repository failed: its forge
// sync, or the update of its checkout.
func syncFailed(rs RepoSync) bool {
	switch rs.Checkout.State {
	case openshell.CheckoutUpdated, openshell.CheckoutCurrent, openshell.CheckoutFetched, openshell.CheckoutMissing:
		return rs.ForgeErr != ""
	}
	return true
}

// syncNotice sums up a sync: how many repositories failed, else how many
// were only fetched (local work kept) or have no checkout yet.
func syncNotice(label string, res []RepoSync) notice {
	failed, fetched, missing := 0, 0, 0
	for _, rs := range res {
		switch {
		case syncFailed(rs):
			failed++
		case rs.Checkout.State == openshell.CheckoutFetched:
			fetched++
		case rs.Checkout.State == openshell.CheckoutMissing:
			missing++
		}
	}
	if failed > 0 {
		return failure(fmt.Sprintf("%s: %d of %d failed to sync.", label, failed, len(res)))
	}
	var parts []string
	if fetched > 0 {
		parts = append(parts, fmt.Sprintf("%d only fetched", fetched))
	}
	if missing > 0 {
		parts = append(parts, fmt.Sprintf("%d not cloned", missing))
	}
	if len(parts) == 0 {
		return success("Synced " + label + ".")
	}
	return success("Synced " + label + "; " + strings.Join(parts, ", ") + ".")
}
