package selection

import (
	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/components/prompt"
)

// repoDeletedMsg is the outcome of deleting a repository.
type repoDeletedMsg struct {
	repo string
	err  error
}

// canDelete reports whether the user may delete repo, as `repo delete`
// decides: read access and collaborator write access.
func (s *Selection) canDelete(repo string) bool {
	be := s.common.Backend()
	if repo == "" || be == nil {
		return false
	}
	ctx := s.common.Context()
	return be.AccessLevelForUser(ctx, repo, proto.UserFromContext(ctx)) >= access.ReadWriteAccess
}

// openDelete asks to delete repo (the Repositories tab's d key).
func (s *Selection) openDelete(repo string) {
	s.repoNotice = notice{}
	if !s.canDelete(repo) {
		if repo != "" {
			s.repoNotice = unauthorizedNotice(repo)
		}
		return
	}
	s.deleteTarget = repo
	s.deleting = prompt.NewConfirm(s.common, "delete", "delete?", "loses history, collaborators, webhooks")
}

// onDeleteKey answers the delete confirm: y deletes in the background.
func (s *Selection) onDeleteKey(msg tea.KeyPressMsg) tea.Cmd {
	res := s.deleting.Update(msg)
	if res == prompt.Pending {
		return nil
	}
	s.deleting = nil
	repo := s.deleteTarget
	s.deleteTarget = ""
	if res != prompt.Submitted {
		s.repoNotice = keptNotice(repo)
		return nil
	}
	ctx := s.common.Context()
	be := s.common.Backend()
	user := proto.UserFromContext(ctx)
	s.repoNotice = deletingNotice(repo)
	return func() tea.Msg {
		// Checked again: access may have changed since the confirm opened.
		if be.AccessLevelForUser(ctx, repo, user) < access.ReadWriteAccess {
			return repoDeletedMsg{repo: repo, err: proto.ErrUnauthorized}
		}
		return repoDeletedMsg{repo: repo, err: be.DeleteRepository(ctx, repo)}
	}
}

// onRepoDeleted reports the outcome and lists the repositories again.
func (s *Selection) onRepoDeleted(msg repoDeletedMsg) tea.Cmd {
	if msg.err != nil {
		s.common.Logger.Info("selection: cannot delete repository", "repo", msg.repo, "err", msg.err)
		s.repoNotice = failedNotice(msg.repo, msg.err)
		return nil
	}
	s.repoNotice = deletedNotice(msg.repo)
	return s.reloadRepos("")
}
