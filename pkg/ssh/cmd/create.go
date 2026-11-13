package cmd

import (
	"errors"
	"fmt"

	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/spf13/cobra"
)

// errStartNeedsSessions is returned for `repo create --start` on a server
// without sessions.
var errStartNeedsSessions = errors.New("--start needs agent sessions, which are off on this server (openshell.enabled)")

// createCommand is the command for creating a new repository.
func createCommand() *cobra.Command {
	var private bool
	var description string
	var projectName string
	var hidden bool
	var vcsFlag string
	var template, profile string
	var start bool

	cmd := &cobra.Command{
		Use:   "create REPOSITORY",
		Short: "Create a new repository",
		Long: `Create a new repository.

--template NAME initialises it from one of the server's templates
(projects.templates in the config): the template repository's tree, at its
configured ref and subdirectory, becomes the repository's single initial
commit, authored by you. You need read access to the template repository.

--profile NAME stores the session profile new agent sessions of the
repository start from (see "repo profile"); a template may set one too.
--start then starts a session for you, detached ("session start --detach").`,
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: checkIfRepoCollab,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg := config.FromContext(ctx)
			be := backend.FromContext(ctx)
			user := proto.UserFromContext(ctx)
			name := args[0]
			vcs, err := proto.ParseVCS(vcsFlag)
			if err != nil {
				return err
			}
			if start {
				// Checked before anything is created.
				if !cfg.OpenShell.Enabled {
					return errStartNeedsSessions
				}
				if user == nil {
					return fmt.Errorf("--start: %w", proto.ErrUnauthorized)
				}
			}
			r, err := be.CreateProject(ctx, name, user, backend.ProjectOptions{
				RepositoryOptions: proto.RepositoryOptions{
					Private:     private,
					Description: description,
					ProjectName: projectName,
					Hidden:      hidden,
					Vcs:         vcs,
				},
				Template: template,
				Profile:  profile,
			})
			if err != nil {
				return err
			}

			cloneurl := fmt.Sprintf("%s/%s.git", cfg.SSH.PublicURL, r.Name())
			cmd.PrintErrf("Created repository %s\n", r.Name())
			if template != "" {
				cmd.PrintErrf("Initialised from template %s\n", template)
			}
			if p, err := be.RepoProfile(ctx, r.Name()); err == nil && p != "" {
				cmd.PrintErrf("Session profile: %s\n", p)
			}
			cmd.Println(proto.CloneCommand(r.Vcs(), cloneurl))

			if start {
				// The repository's profile is resolved as for any new
				// session: --profile, the template's, sessions.repos.
				if err := runSession(cmd, sessionctl.Request{Repo: r.Name(), Profile: profile, Create: true}, true); err != nil {
					return fmt.Errorf("repository %s was created, but its session did not start: %w", r.Name(), err)
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&private, "private", "p", false, "make the repository private")
	cmd.Flags().StringVarP(&description, "description", "d", "", "set the repository description")
	cmd.Flags().StringVarP(&projectName, "name", "n", "", "set the project name")
	cmd.Flags().BoolVarP(&hidden, "hidden", "H", false, "hide the repository from the UI")

	cmd.Flags().StringVarP(&vcsFlag, "vcs", "", string(proto.DefaultVCS), "set the repository VCS kind (git or jj)")
	cmd.Flags().StringVarP(&template, "template", "t", "", "initialise the repository from a template (projects.templates)")
	cmd.Flags().StringVar(&profile, "profile", "", "set the repository's default session profile (sessions.profiles)")
	cmd.Flags().BoolVar(&start, "start", false, "start a detached agent session for the repository once it is created")

	return cmd
}
