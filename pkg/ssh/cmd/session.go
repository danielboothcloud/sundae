package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sessionctl"
	"github.com/charmbracelet/soft-serve/pkg/signals"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/spf13/cobra"
)

// errNoTerminal is returned when a command that attaches runs without a PTY.
var errNoTerminal = errors.New("requires a terminal (ssh -t)")

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
		sessionSignalsCommand(),
		sessionParkCommand(),
		sessionResumeCommand(),
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
session. With --detach, start the session without attaching. A parked
session is resumed first.`,
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

The session is resumed first if it is parked, and its program is started
again if it is not running. Detach with ~. (after a newline), or close the
connection; the program keeps running.`,
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

// newManager returns a Manager for a command, publishing on the context's
// event bus and coordinating with the context's Monitor.
func newManager(ctx context.Context, client openshell.Client) *openshell.Manager {
	return sessionctl.NewManager(ctx, client)
}

// runSession starts (create) or resumes the session, then attaches the SSH
// terminal to it unless detach is set.
func runSession(cmd *cobra.Command, repo string, opts openshell.StartOptions, create, detach bool) error {
	ctx := cmd.Context()
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	sess := sshutils.SessionFromContext(ctx)

	if _, err := be.Repository(ctx, repo); err != nil {
		return err
	}

	client, err := newOpenShellClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck

	req := sessionctl.Request{
		Repo:     repo,
		Name:     opts.Name,
		Profile:  opts.Profile,
		Image:    opts.Image,
		Create:   create,
		Progress: cmd.ErrOrStderr(),
	}
	if sess != nil {
		if pty, _, ok := sess.Pty(); ok {
			req.Term = pty.Term
		}
	}

	p, err := sessionctl.Prepare(ctx, newManager(ctx, client), req)
	if err != nil {
		return err
	}
	defer p.Close()

	if detach {
		s := p.Session
		cmd.PrintErrf("Session %s of %s is running in sandbox %s.\n", s.Name, s.Repo, s.Sandbox)
		cmd.PrintErrf("Attach with: session attach %s%s\n", s.Repo, nameArg(s.Name))
		return nil
	}

	return attach(ctx, p, sess)
}

// attach bridges the SSH session to the osewa session and propagates the
// exit code of the attach client: the program's exit code when it ended, 0
// when the user detached.
func attach(ctx context.Context, p *sessionctl.Prepared, sess sshSession) error {
	code, err := p.Attach(ctx, sess)
	if err != nil {
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

// sessionRow is one session of `session ls`.
type sessionRow = sessionctl.Row

// sessionColumns are the columns of `session ls`.
var sessionColumns = []struct {
	header string
	value  func(sessionRow, time.Time) string
}{
	{"Repository", func(r sessionRow, _ time.Time) string { return r.Info.Repo }},
	{"Name", func(r sessionRow, _ time.Time) string { return r.Info.Name }},
	{"Profile", func(r sessionRow, _ time.Time) string { return orDash(r.Info.Profile) }},
	{"Sandbox", func(r sessionRow, _ time.Time) string { return r.Info.Sandbox }},
	{"Phase", func(r sessionRow, _ time.Time) string { return orDash(r.Info.Phase) }},
	{"State", func(r sessionRow, _ time.Time) string { return r.Label() }},
	{"Message", func(r sessionRow, _ time.Time) string { return orDash(signals.RecordMessage(r.Record(), maxMessage)) }},
	{"Attached", func(r sessionRow, _ time.Time) string {
		if !r.Polled() {
			return "-"
		}
		return strconv.Itoa(r.Status.Attached)
	}},
	{"Changed", func(r sessionRow, now time.Time) string {
		if r.Status == nil {
			return "-"
		}
		return sessionctl.Since(r.Status.Changed, now)
	}},
}

// maxMessage is how many characters of a message `session ls` shows.
const maxMessage = 60

// sessionJSON is one session of `session ls --json`. Text from the program
// (app, title, msg) is sanitised but not cut.
type sessionJSON struct {
	Repo     string `json:"repo"`
	Name     string `json:"name"`
	Profile  string `json:"profile,omitempty"`
	Sandbox  string `json:"sandbox"`
	Phase    string `json:"phase"`
	Parked   bool   `json:"parked"`
	State    string `json:"state"`
	Kind     string `json:"kind,omitempty"`
	Progress *int   `json:"progress,omitempty"`
	App      string `json:"app,omitempty"`
	Title    string `json:"title,omitempty"`
	Msg      string `json:"msg,omitempty"`
	// Attached is nil until the status has been read.
	Attached *int       `json:"attached,omitempty"`
	Bridges  int        `json:"bridges"`
	Running  bool       `json:"running"`
	Exited   bool       `json:"exited"`
	ExitCode *int       `json:"exit_code,omitempty"`
	Changed  *time.Time `json:"changed,omitempty"`
	Polled   *time.Time `json:"polled,omitempty"`
	Stale    bool       `json:"stale"`
	Error    string     `json:"error,omitempty"`
	// Unsuspendable is why the gateway refused to park the session.
	Unsuspendable string `json:"unsuspendable,omitempty"`
}

func sessionToJSON(r sessionRow) sessionJSON {
	rec := r.Record()
	j := sessionJSON{
		Repo:     r.Info.Repo,
		Name:     r.Info.Name,
		Profile:  r.Info.Profile,
		Sandbox:  r.Info.Sandbox,
		Phase:    r.Info.Phase,
		Parked:   r.Parked(),
		State:    string(rec.State),
		Kind:     signals.Sanitize(string(rec.Kind), 0),
		Progress: rec.Progress,
		App:      signals.Sanitize(rec.App, 0),
		Title:    signals.Sanitize(rec.Title, 0),
		Msg:      signals.Sanitize(rec.Msg, 0),
	}
	if j.State == "" {
		j.State = "none"
	}
	if st := r.Status; st != nil {
		j.Bridges = st.Bridges
		j.Running, j.Exited, j.ExitCode = st.Running, st.Exited, st.ExitCode
		j.Stale, j.Error, j.Unsuspendable = st.Stale, signals.Sanitize(st.Error, 0), st.Unsuspendable
		if !st.Changed.IsZero() {
			j.Changed = &st.Changed
		}
		if r.Polled() {
			j.Polled = &st.Polled
			a := st.Attached
			j.Attached = &a
		}
	}
	return j
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sessionListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List your agent sessions and what their agents are doing",
		Long: `List your agent sessions: repository, name, profile, sandbox, phase,
and the agent's status (OSC 7501) as Soft Serve last read it: state, message,
how many clients are attached, and when the state last changed.

Sessions that need you come first: blocked, error, done, working, idle,
then those with no status. A parked session keeps the state it had when it
was parked. Listing never resumes a parked session.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := config.FromContext(ctx)
			user := proto.UserFromContext(ctx)

			client, err := newOpenShellClient(cfg)
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			rows, err := sessionctl.List(ctx, newManager(ctx, client), openshell.MonitorFromContext(ctx), user)
			if err != nil {
				return err
			}

			if asJSON {
				out := make([]sessionJSON, len(rows))
				for i, r := range rows {
					out[i] = sessionToJSON(r)
				}
				return writeJSON(cmd, out)
			}

			if len(rows) == 0 {
				cmd.Println("No sessions found")
				return nil
			}

			now := time.Now()
			headers := make([]string, len(sessionColumns))
			for i, c := range sessionColumns {
				headers[i] = c.header
			}
			t := table.New().Headers(headers...)
			for _, r := range rows {
				row := make([]string, len(sessionColumns))
				for i, c := range sessionColumns {
					row[i] = c.value(r, now)
				}
				t = t.Row(row...)
			}
			cmd.Println(t)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func sessionParkCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "park REPOSITORY",
		Short: "Park (suspend) an agent session now",
		Long: `Park an agent session: suspend its sandbox, keeping its memory where the
OpenShell compute driver supports it, so it uses no CPU until it is resumed.
"session start", "session attach" and "session resume" resume it.

A session with an open bridge is not parked; detach first. Sessions are also
parked automatically after sessions.suspend.after with nobody attached and
no working status.`,
		Args:    cobra.ExactArgs(1),
		PreRunE: checkIfReadable,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := newOpenShellClient(config.FromContext(ctx))
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			// Like end, no repository lookup: repoArg is the name the
			// sandbox name derives from.
			repo := repoArg(args)
			parked, err := newManager(ctx, client).Park(ctx, proto.UserFromContext(ctx), repo, name)
			if err != nil {
				return err
			}
			if name == "" {
				name = openshell.DefaultSessionName
			}
			if parked {
				cmd.PrintErrf("Session %s of %s parked. Resume it with: session resume %s%s\n", name, repo, repo, nameArg(name))
			} else {
				cmd.PrintErrf("Session %s of %s is already parked\n", name, repo)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", `Session name (default "main")`)
	return cmd
}

func sessionResumeCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "resume REPOSITORY",
		Short: "Resume a parked agent session without attaching",
		Long: `Resume an agent session without attaching to it: resume its sandbox if it
is parked, renew its git credentials, and start its program again if it is
not running. Like "session attach --detach"; it never creates a session.`,
		Args:    cobra.ExactArgs(1),
		PreRunE: checkIfReadable,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSession(cmd, args[0], openshell.StartOptions{Name: name}, false, true)
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", `Session name (default "main")`)
	return cmd
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
			client, err := newOpenShellClient(config.FromContext(ctx))
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			// No repository lookup: a session must stay endable after its
			// repository is deleted or renamed. repoArg is the same
			// sanitized name Start derives the sandbox name from.
			mgr := newManager(ctx, client)
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
