package selection

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/ui/components/prompt"
	"github.com/danielboothcloud/sundae/pkg/utils"
)

// repoCreatedMsg is the outcome of the new-repository form.
type repoCreatedMsg struct {
	repo    string
	profile string
	start   bool
	err     error
}

// canCreate reports whether the user may create repositories, as `repo
// create` decides: write access to a repository that does not exist yet,
// which every user has, and anonymous users only with anon-access
// read-write or above.
func (s *Selection) canCreate() bool {
	be := s.common.Backend()
	if be == nil || s.common.Config() == nil {
		return false
	}
	ctx := s.common.Context()
	if user := proto.UserFromContext(ctx); user != nil {
		return true
	}
	return be.AnonAccess(ctx) >= access.ReadWriteAccess
}

// openNewRepo opens the new-repository form (the Repositories tab's n
// key).
func (s *Selection) openNewRepo() {
	s.repoNotice = ""
	s.repoForm = s.newRepoForm()
}

// newRepoForm returns the new-repository form: what `repo create` takes,
// with the template and profile fields shown only when some are
// configured, and the start-session toggle only when sessions are on.
func (s *Selection) newRepoForm() *prompt.Form {
	ctx := s.common.Context()
	be := s.common.Backend()
	cfg := s.common.Config()
	user := proto.UserFromContext(ctx)

	name := prompt.TextField("name", "Name", "", func(v string) error {
		if v == "" {
			return errors.New("enter a name")
		}
		n := utils.SanitizeRepo(v)
		if err := utils.ValidateRepo(n); err != nil {
			return err //nolint:wrapcheck
		}
		if _, err := be.Repository(ctx, n); err == nil {
			return fmt.Errorf("%s already exists", n)
		}
		return nil
	})
	name.SetCharLimit(128)
	desc := prompt.TextField("description", "Description", "", nil)
	desc.SetCharLimit(256)
	vcs := prompt.ChoiceField("vcs", "VCS", []prompt.Option{
		{Value: string(proto.VCSJJ), Detail: "jj git clone"},
		{Value: string(proto.VCSGit), Detail: "git clone"},
	}, string(proto.DefaultVCS), false)
	fields := []*prompt.Field{name, desc, vcs}

	// Templates the user may read: the others would be refused.
	topts := []prompt.Option{{Value: "", Label: "none", Detail: "empty repository"}}
	profileOf := map[string]string{}
	for _, n := range cfg.Projects.TemplateNames() {
		t := cfg.Projects.Templates[n]
		if _, err := be.Repository(ctx, t.Repo); err != nil || be.AccessLevelForUser(ctx, t.Repo, user) < access.ReadOnlyAccess {
			continue
		}
		detail := t.Description
		if detail == "" {
			detail = t.Repo
		}
		topts = append(topts, prompt.Option{Value: n, Detail: signals.Sanitize(detail, 50)})
		profileOf[n] = t.Profile
	}
	if len(topts) > 1 {
		fields = append(fields, prompt.ChoiceField("template", "Template", topts, "", false))
	}

	if len(cfg.Sessions.Profiles) > 0 {
		popts := []prompt.Option{{Value: "", Label: "none", Detail: "sessions.repos, else the default"}}
		for _, p := range sortedProfiles(cfg.Sessions.Profiles) {
			popts = append(popts, prompt.Option{Value: p.name, Detail: signals.Sanitize(p.detail, 40)})
		}
		fields = append(fields, prompt.ChoiceField("profile", "Profile", popts, "", false))
	}

	fields = append(fields, prompt.ToggleField("private", "Private", false))
	if s.sessions != nil {
		fields = append(fields, prompt.ToggleField("start", "Start session", false))
	}

	f := prompt.NewForm(s.common, "New repository", "create", fields...)
	profileTouched := false
	f.OnChange = func(f *prompt.Form, key string) {
		switch key {
		case "profile":
			profileTouched = true
		case "template":
			// A template's profile, until the user picks one.
			if p := f.Field("profile"); p != nil && !profileTouched {
				p.SetValue(profileOf[f.Field("template").Value()])
			}
		}
	}
	return f
}

// fieldValue returns a form field's value; "" when the form has no such
// field.
func fieldValue(f *prompt.Form, key string) string {
	if fd := f.Field(key); fd != nil {
		return fd.Value()
	}
	return ""
}

// createRepoCmd creates the repository the form describes, in the
// background.
func (s *Selection) createRepoCmd(f *prompt.Form) tea.Cmd {
	ctx := s.common.Context()
	be := s.common.Backend()
	user := proto.UserFromContext(ctx)
	name := utils.SanitizeRepo(fieldValue(f, "name"))
	vcs, _ := proto.ParseVCS(fieldValue(f, "vcs"))
	opts := backend.ProjectOptions{
		RepositoryOptions: proto.RepositoryOptions{
			Description: fieldValue(f, "description"),
			Private:     fieldValue(f, "private") == "yes",
			Vcs:         vcs,
		},
		Template: fieldValue(f, "template"),
		Profile:  fieldValue(f, "profile"),
	}
	start := fieldValue(f, "start") == "yes"
	s.creating = name
	return func() tea.Msg {
		// The check `repo create` makes.
		if be.AccessLevelForUser(ctx, name, user) < access.ReadWriteAccess {
			return repoCreatedMsg{repo: name, err: proto.ErrUnauthorized}
		}
		r, err := be.CreateProject(ctx, name, user, opts)
		if err != nil {
			return repoCreatedMsg{repo: name, err: err}
		}
		return repoCreatedMsg{repo: r.Name(), profile: opts.Profile, start: start}
	}
}

// onRepoCreated closes the form and selects the new repository, then
// starts its session when asked; an error keeps the form open with it.
func (s *Selection) onRepoCreated(msg repoCreatedMsg) tea.Cmd {
	s.creating = ""
	if msg.err != nil {
		if s.repoForm != nil {
			s.repoForm.SetError(msg.err)
			return nil
		}
		s.repoNotice = "✗ " + msg.repo + ": " + msg.err.Error()
		return nil
	}
	s.repoForm = nil
	s.repoNotice = "Created " + msg.repo + "."
	cmd := s.reloadRepos(msg.repo)
	if msg.start && s.sessions != nil {
		start := s.selectPane(sessionsPane)
		s.bindSessions()
		return tea.Batch(cmd, start, s.sessions.StartNew(msg.repo, msg.profile, true))
	}
	return cmd
}

// reloadRepos lists the repositories again and selects the one named
// selected.
func (s *Selection) reloadRepos(selected string) tea.Cmd {
	items, err := s.repoItems()
	if err != nil {
		return nil
	}
	s.selector.ResetFilter()
	cmd := s.selector.SetItems(items)
	for i, it := range items {
		if it.ID() == selected {
			s.selector.Select(i)
		}
	}
	return cmd
}

// refreshRepos lists the repositories again after a change made
// elsewhere, such as an import. It keeps the selected repository and the
// user's filter.
func (s *Selection) refreshRepos() tea.Cmd {
	items, err := s.repoItems()
	if err != nil {
		s.common.Logger.Debug("ui: cannot list repositories", "err", err)
		return nil
	}
	selected := s.SelectedRepo()
	cmd := s.selector.SetItems(items)
	if s.FilterState() == list.Unfiltered {
		for i, it := range items {
			if it.ID() == selected {
				s.selector.Select(i)
			}
		}
	}
	return cmd
}

// listed reports whether the Repositories tab lists the repository.
func (s *Selection) listed(repo string) bool {
	for _, it := range s.selector.Items() {
		if it, ok := it.(Item); ok && it.ID() == repo {
			return true
		}
	}
	return false
}

// profileInfo is a configured session profile, as the form offers it.
type profileInfo struct{ name, detail string }

// sortedProfiles returns the profiles sorted by name, each with its image
// and command.
func sortedProfiles(m map[string]config.SessionProfile) []profileInfo {
	out := make([]profileInfo, 0, len(m))
	for _, name := range slices.Sorted(maps.Keys(m)) {
		p := m[name]
		var parts []string
		for _, v := range []string{p.Image, p.Command} {
			if v != "" {
				parts = append(parts, v)
			}
		}
		out = append(out, profileInfo{name: name, detail: strings.Join(parts, " · ")})
	}
	return out
}

// onRepoFormKey gives a key to the open new-repository form. While the
// repository is being created, keys are ignored.
func (s *Selection) onRepoFormKey(msg tea.KeyPressMsg) tea.Cmd {
	if s.creating != "" {
		return nil
	}
	res, cmd := s.repoForm.Update(msg)
	switch res {
	case prompt.Submitted:
		return s.createRepoCmd(s.repoForm)
	case prompt.Cancelled:
		s.repoForm = nil
	}
	return cmd
}

// SelectedRepo returns the name of the repository selected on the
// Repositories tab; "" for none.
func (s *Selection) SelectedRepo() string {
	if it, ok := s.selector.SelectedItem().(Item); ok {
		return it.ID()
	}
	return ""
}
