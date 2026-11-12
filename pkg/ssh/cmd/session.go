package cmd

import (
	"context"
	"errors"
	"fmt"

	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/spf13/cobra"
)

// errNoTerminal is returned when a command that attaches runs without a PTY.
var errNoTerminal = errors.New("requires a terminal (ssh -t)")

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
		sessionAttachCommand(),
		sessionListCommand(),
		sessionEndCommand(),
	)

	return cmd
}

func sessionStartCommand() *cobra.Command {
	var name, profile, image string
	var detach bool
	cmd := &cobra.Command{
		Use:   "start REPOSITORY",
		Short: "Start or resume an agent session for a repository (requires ssh -t unless --detach)",
		Long: `Start or resume an agent session for a repository, then attach to it.

Each session has its own OpenShell sandbox with the repository cloned into
/sandbox/<repo>, and runs its program (openshell.command, or the profile's)
in an osewa session there. Sessions are named per repository: --name picks
one, "main" by default, so several agents can work on one repository.

A new session is created from --profile, else from the profile
sessions.repos assigns to the repository, else from openshell.image and
openshell.command; --image overrides the image. Both only apply when the
session is created.

Detach with ~. (after a newline), or just close the connection: the program
keeps running. "session attach" re-attaches and "session end" deletes the
session. With --detach, start the session without attaching.`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if !detach {
				if err := checkTerminal(cmd); err != nil {
					return fmt.Errorf("%w, or --detach", err)
				}
			}
			return checkIfReadable(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSession(cmd, args[0], openshell.StartOptions{
				Name:    name,
				Profile: profile,
				Image:   image,
			}, true, detach)
		},
	}

	cmd.Flags().StringVarP(&name, "name", "n", "", `Session name (default "main")`)
	cmd.Flags().StringVarP(&profile, "profile", "p", "", "Profile to create the session from (ignored when it already exists)")
	cmd.Flags().StringVar(&image, "image", "", "Sandbox image to use when the sandbox is created (ignored when it already exists)")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "Start the session without attaching to it")

	return cmd
}

func sessionAttachCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "attach REPOSITORY",
		Short: "Attach to a running agent session (requires ssh -t)",
		Long: `Attach to an existing agent session of a repository.

The session's program is started again if it is not running. Detach with ~.
(after a newline), or close the connection; the program keeps running.`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := checkTerminal(cmd); err != nil {
				return err
			}
			return checkIfReadable(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSession(cmd, args[0], openshell.StartOptions{Name: name}, false, false)
		},
	}

	cmd.Flags().StringVarP(&name, "name", "n", "", `Session name (default "main")`)

	return cmd
}

// runSession starts (create) or resumes the session, then attaches the SSH
// terminal to it unless detach is set.
func runSession(cmd *cobra.Command, repo string, opts openshell.StartOptions, create, detach bool) error {
	ctx := cmd.Context()
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	sess := sshutils.SessionFromContext(ctx)

	rr, err := be.Repository(ctx, repo)
	if err != nil {
		return err
	}

	client, err := newOpenShellClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck

	mgr := openshell.NewManager(client, be, cfg, openshell.WithEvents(events.FromContext(ctx)))
	opts.User = proto.UserFromContext(ctx)
	opts.Repo = rr.Name()
	opts.VCS = rr.Vcs()
	opts.Progress = cmd.ErrOrStderr()
	if sess != nil {
		if pty, _, ok := sess.Pty(); ok {
			opts.Term = pty.Term
		}
	}

	var s *openshell.Session
	if create {
		s, err = mgr.Start(ctx, opts)
	} else {
		s, err = mgr.Resume(ctx, opts)
	}
	if err != nil {
		return err
	}

	if detach {
		cmd.PrintErrf("Session %s of %s is running in sandbox %s.\n", s.Name, s.Repo, s.Sandbox)
		cmd.PrintErrf("Attach with: session attach %s%s\n", s.Repo, nameArg(s.Name))
		return nil
	}

	return attach(ctx, mgr, s, sess)
}

// attach bridges the SSH session to the osewa session and propagates the
// exit code of the attach client: the program's exit code when it ended, 0
// when the user detached.
func attach(ctx context.Context, mgr *openshell.Manager, s *openshell.Session, sess sshSession) error {
	pty, _, _ := sess.Pty()
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
		// Propagate the exit status to the SSH client.
		_ = sess.Exit(code)
	}
	return nil
}

// sshSession is the part of ssh.Session attach uses.
type sshSession interface {
	openshell.Terminal
	Exit(code int) error
}

// nameArg returns the --name argument selecting a session, or "" for the
// default session.
func nameArg(name string) string {
	if name == "" || name == openshell.DefaultSessionName {
		return ""
	}
	return " --name " + name
}

// sessionColumns are the columns of `session ls`. Status columns from the
// sessions' osewa status go at the end.
var sessionColumns = []struct {
	header string
	value  func(openshell.Info) string
}{
	{"Repository", func(i openshell.Info) string { return i.Repo }},
	{"Name", func(i openshell.Info) string { return i.Name }},
	{"Profile", func(i openshell.Info) string { return orDash(i.Profile) }},
	{"Sandbox", func(i openshell.Info) string { return i.Sandbox }},
	{"Phase", func(i openshell.Info) string { return orDash(i.Phase) }},
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

			headers := make([]string, len(sessionColumns))
			for i, c := range sessionColumns {
				headers[i] = c.header
			}
			t := table.New().Headers(headers...)
			for _, info := range infos {
				row := make([]string, len(sessionColumns))
				for i, c := range sessionColumns {
					row[i] = c.value(info)
				}
				t = t.Row(row...)
			}
			cmd.Println(t)
			return nil
		},
	}
}

func sessionEndCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:     "end REPOSITORY",
		Aliases: []string{"stop", "rm"},
		Short:   "End an agent session, deleting its sandbox",
		Long: `End an agent session, deleting its sandbox and revoking its git
credentials. Other sessions of the repository are left running.`,
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
			existed, err := mgr.End(ctx, proto.UserFromContext(ctx), repoArg(args), name)
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

	cmd.Flags().StringVarP(&name, "name", "n", "", `Session name (default "main")`)

	return cmd
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
	if sess != nil {
		if _, _, ok := sess.Pty(); ok {
			return nil
		}
	}
	return fmt.Errorf("session %s %w", cmd.Name(), errNoTerminal)
}
