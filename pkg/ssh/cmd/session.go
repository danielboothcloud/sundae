package cmd

import (
	"errors"

	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/spf13/cobra"
)

// errNoTerminal is returned when `session start` runs without a PTY.
var errNoTerminal = errors.New("session start requires a terminal (ssh -t)")

// errSessionLost is returned when the sandbox stream ends without reporting
// an exit status.
var errSessionLost = errors.New("session ended without an exit status")

// newOpenShellClient creates the OpenShell client for a command invocation.
// Tests may replace it.
var newOpenShellClient = func(cfg *config.Config) (openshell.Client, error) {
	return openshell.New(cfg.OpenShell)
}

// SessionCommand returns the command that manages OpenShell agent sessions.
// It is only registered when OpenShell is enabled.
func SessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "session",
		Short:             "Manage OpenShell agent sessions",
		PersistentPreRunE: checkIfUser,
	}

	cmd.AddCommand(
		sessionStartCommand(),
		sessionListCommand(),
		sessionEndCommand(),
	)

	return cmd
}

func sessionStartCommand() *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:   "start REPOSITORY",
		Short: "Start or resume an agent session for a repository (requires ssh -t)",
		Long: `Start or resume an agent session for a repository.

Each user gets one OpenShell sandbox per repository with the repository
cloned into /sandbox/<repo>. Exiting the shell leaves the sandbox running;
run "session start" again to re-attach and "session end" to delete it.`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := checkTerminal(cmd); err != nil {
				return err
			}
			return checkIfReadable(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg := config.FromContext(ctx)
			be := backend.FromContext(ctx)
			user := proto.UserFromContext(ctx)
			sess := sshutils.SessionFromContext(ctx)

			rr, err := be.Repository(ctx, repoArg(args))
			if err != nil {
				return err
			}

			client, err := newOpenShellClient(cfg)
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			mgr := openshell.NewManager(client, be, cfg, openshell.WithEvents(events.FromContext(ctx)))
			pty, _, _ := sess.Pty()
			s, err := mgr.Start(ctx, openshell.StartOptions{
				User:     user,
				Repo:     rr.Name(),
				VCS:      rr.Vcs(),
				Image:    image,
				Term:     pty.Term,
				Progress: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			is, err := mgr.Attach(ctx, s, uint32(pty.Window.Width), uint32(pty.Window.Height)) //nolint:gosec
			if err != nil {
				return err
			}

			code, err := openshell.Bridge(ctx, sess, is)
			if code < 0 && ctx.Err() == nil {
				// The stream ended without an exit status: surface why.
				if err == nil {
					err = errSessionLost
				}
				return err
			}
			if err != nil && ctx.Err() == nil && code == 0 {
				return err
			}
			if code > 0 {
				// Propagate the shell's exit status to the SSH client.
				_ = sess.Exit(code)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&image, "image", "", "Sandbox image to use when the sandbox is created (ignored when it already exists)")

	return cmd
}

func sessionListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List your agent sessions",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := config.FromContext(ctx)
			be := backend.FromContext(ctx)

			client, err := newOpenShellClient(cfg)
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			infos, err := openshell.NewManager(client, be, cfg).List(ctx, proto.UserFromContext(ctx))
			if err != nil {
				return err
			}

			if len(infos) == 0 {
				cmd.Println("No sessions found")
				return nil
			}

			t := table.New().Headers("Repository", "Sandbox", "Phase")
			for _, info := range infos {
				t = t.Row(info.Repo, info.Name, info.Phase)
			}
			cmd.Println(t)
			return nil
		},
	}
}

func sessionEndCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "end REPOSITORY",
		Aliases: []string{"stop", "rm"},
		Short:   "End an agent session, deleting its sandbox",
		Args:    cobra.ExactArgs(1),
		PreRunE: checkIfReadable,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg := config.FromContext(ctx)
			be := backend.FromContext(ctx)

			client, err := newOpenShellClient(cfg)
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			// No repository lookup: a session must stay endable after its
			// repository is deleted or renamed. repoArg is the same
			// sanitized name Start derives the sandbox name from.
			mgr := openshell.NewManager(client, be, cfg, openshell.WithEvents(events.FromContext(ctx)))
			existed, err := mgr.End(ctx, proto.UserFromContext(ctx), repoArg(args))
			if err != nil {
				return err
			}

			if existed {
				cmd.PrintErrln("Session ended")
			} else {
				cmd.PrintErrln("No session to end")
			}
			return nil
		},
	}
}

// checkIfUser rejects anonymous callers.
func checkIfUser(cmd *cobra.Command, _ []string) error {
	if proto.UserFromContext(cmd.Context()) == nil {
		return proto.ErrUnauthorized
	}
	return nil
}

// checkTerminal requires the SSH session to have a PTY.
func checkTerminal(cmd *cobra.Command) error {
	sess := sshutils.SessionFromContext(cmd.Context())
	if sess == nil {
		return errNoTerminal
	}
	if _, _, ok := sess.Pty(); !ok {
		return errNoTerminal
	}
	return nil
}
