package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2/table"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
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
		sessionNewCommand(),
		sessionStartCommand(),
		sessionAttachCommand(),
		sessionReposCommand(),
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
	var detach, fresh, restore bool
	cmd := &cobra.Command{
		Use:   "start REPOSITORY",
		Short: "Start or resume a session for one repository (requires ssh -t unless --detach)",
		Long: `Start or resume an agent session holding a repository, then attach to it:
the one-repository shorthand of "session new".

The session is named after the repository's last path segment unless --name
says otherwise; when a session of that name holds other repositories, a
numbered name (tool-2) is used. If the session exists, it is resumed and
gets the repository added if it lacks it.

` + sessionCreateHelp,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := checkStartFlags(cmd, detach, fresh, restore); err != nil {
				return err
			}
			return checkIfReadable(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := sessionctl.Request{Repo: args[0], Name: name, Profile: profile, Image: image, Create: true}
			req.Restore = restoreMode(fresh, restore)
			return runSession(cmd, req, detach)
		},
	}
	addCreateFlags(cmd, &name, &profile, &image, &detach, &fresh, &restore)
	return cmd
}

func sessionNewCommand() *cobra.Command {
	var repos []string
	var profile, image string
	var detach, fresh, restore bool
	cmd := &cobra.Command{
		Use:   "new NAME --repo REPOSITORY [--repo REPOSITORY...]",
		Short: "Create (or resume) a named session holding one or more repositories",
		Long: `Create an agent session called NAME holding the repositories given with
--repo, each cloned into /sandbox/<repository>, then attach to it. Names are
1 to 32 lowercase letters, digits and hyphens, unique per user.

If the session exists, it is resumed and gets the repositories it lacks
added. "session repos" adds and removes repositories later.

` + sessionCreateHelp,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if len(repos) == 0 {
				return errors.New("name at least one repository with --repo")
			}
			if !openshell.ValidSessionName(args[0]) {
				return openshell.ErrInvalidSessionName
			}
			if err := checkStartFlags(cmd, detach, fresh, restore); err != nil {
				return err
			}
			for _, r := range repos {
				if err := checkIfReadable(cmd, []string{r}); err != nil {
					return fmt.Errorf("%s: %w", r, err)
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := sessionctl.Request{Repos: repos, Name: args[0], Profile: profile, Image: image, Create: true}
			req.Restore = restoreMode(fresh, restore)
			return runSession(cmd, req, detach)
		},
	}
	cmd.Flags().StringArrayVarP(&repos, "repo", "r", nil, "Repository the session holds (repeat for several)")
	addCreateFlags(cmd, nil, &profile, &image, &detach, &fresh, &restore)
	return cmd
}

// sessionCreateHelp is what start and new say about creating a session.
const sessionCreateHelp = `A new session gets its own OpenShell sandbox and runs its program
(openshell.command, or the profile's) in an osewa session, in the checkout
of its only repository, else in /sandbox. Its profile is --profile, else the
first repository's own profile ("repo profile"), else the first
sessions.repos match among its repositories, else none; --image overrides
the image. Both only apply when the session is created.

Detach with ~. (after a newline), or just close the connection: the program
keeps running. "session attach" re-attaches and "session end" deletes the
session. With --detach, start the session without attaching. A parked
session is resumed first.

With S3 snapshots on (snapshots.s3), a session that has a snapshot (taken by
"session end" or a park) gets /sandbox restored from it when its sandbox is
created. --fresh starts from fresh clones instead; the snapshot is kept
until the next one replaces it. When the new sandbox already holds files
from earlier work, you are asked whether to restore over them; --restore
answers yes.`

// addCreateFlags adds the flags start and new share. name may be nil.
func addCreateFlags(cmd *cobra.Command, name, profile, image *string, detach, fresh, restore *bool) {
	if name != nil {
		cmd.Flags().StringVarP(name, "name", "n", "", "Session name (default: from the repository)")
	}
	cmd.Flags().StringVarP(profile, "profile", "p", "", "Profile to create the session from (ignored when it already exists)")
	cmd.Flags().StringVar(image, "image", "", "Sandbox image to use when the sandbox is created (ignored when it already exists)")
	cmd.Flags().BoolVarP(detach, "detach", "d", false, "Start the session without attaching to it")
	cmd.Flags().BoolVar(fresh, "fresh", false, "Do not restore the session's S3 snapshot into a new sandbox")
	cmd.Flags().BoolVar(restore, "restore", false, "Restore the session's S3 snapshot even over files the new sandbox already holds")
}

// checkStartFlags checks the terminal and the snapshot flags.
func checkStartFlags(cmd *cobra.Command, detach, fresh, restore bool) error {
	if !detach {
		if err := checkTerminal(cmd); err != nil {
			return fmt.Errorf("%w, or --detach", err)
		}
	}
	if fresh && restore {
		return errors.New("--fresh and --restore are mutually exclusive")
	}
	return nil
}

func restoreMode(fresh, restore bool) openshell.RestoreMode {
	switch {
	case fresh:
		return openshell.RestoreSkip
	case restore:
		return openshell.RestoreOver
	}
	return openshell.RestoreAuto
}

// sessionArgHelp says what a SESSION argument names.
const sessionArgHelp = `SESSION is a session name, or a repository held by exactly one of your
sessions.`

func sessionAttachCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "attach SESSION",
		Short: "Attach to a running agent session (requires ssh -t)",
		Long: `Attach to an existing agent session.

The session is resumed first if it is parked, and its program is started
again if it is not running. Detach with ~. (after a newline), or close the
connection; the program keeps running.

` + sessionArgHelp,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return checkTerminal(cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := resolveSession(cmd, args[0], name)
			if err != nil {
				return err
			}
			return runSession(cmd, sessionctl.Request{Name: n}, false)
		},
	}
	addLegacyNameFlag(cmd, &name)
	return cmd
}

// addLegacyNameFlag adds the hidden -n flag of the commands that took a
// repository and a session name before sessions held several
// repositories: with it, the argument is ignored and -n names the session.
func addLegacyNameFlag(cmd *cobra.Command, name *string) {
	cmd.Flags().StringVarP(name, "name", "n", "", "Session name")
	_ = cmd.Flags().MarkHidden("name")
}

// resolveSession returns the session a SESSION argument names; legacy is
// the hidden -n flag, which wins.
func resolveSession(cmd *cobra.Command, arg, legacy string) (string, error) {
	if legacy != "" {
		return legacy, nil
	}
	ctx := cmd.Context()
	client, err := newOpenShellClient(config.FromContext(ctx))
	if err != nil {
		return "", err
	}
	defer client.Close()                                                                     //nolint:errcheck
	return sessionctl.Resolve(ctx, newManager(ctx, client), proto.UserFromContext(ctx), arg) //nolint:wrapcheck
}

func sessionReposCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repos SESSION [add|remove REPOSITORY...]",
		Short: "List, add or remove the repositories of a session",
		Long: `List the repositories of a session, or add or remove some.

Adding clones the repositories into the running session (resuming it first
if parked) without attaching. Removing takes them out of the session's list;
their checkouts stay in /sandbox. A session keeps at least one repository.
The session's program keeps running, in the directory it started in.

` + sessionArgHelp,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveSession(cmd, args[0], "")
			if err != nil {
				return err
			}
			if len(args) == 1 {
				return listSessionRepos(cmd, name)
			}
			if len(args) < 3 {
				return errors.New("name at least one repository to add or remove")
			}
			req := sessionctl.Request{Name: name}
			switch args[1] {
			case "add":
				for _, r := range args[2:] {
					if err := checkIfReadable(cmd, []string{r}); err != nil {
						return fmt.Errorf("%s: %w", r, err)
					}
				}
				req.Repos = args[2:]
			case "remove", "rm":
				req.RemoveRepos = args[2:]
			default:
				return fmt.Errorf("unknown action %q: add or remove", args[1])
			}
			return runSession(cmd, req, true)
		},
	}
	return cmd
}

// listSessionRepos prints the repositories of the user's session.
func listSessionRepos(cmd *cobra.Command, name string) error {
	ctx := cmd.Context()
	client, err := newOpenShellClient(config.FromContext(ctx))
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck
	infos, err := newManager(ctx, client).List(ctx, proto.UserFromContext(ctx))
	if err != nil {
		return err
	}
	for _, i := range infos {
		if i.Name == name {
			for _, r := range i.Repos {
				cmd.Println(r)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %q", openshell.ErrNoSession, name)
}

// newManager returns a Manager for a command, publishing on the context's
// event bus and coordinating with the context's Monitor.
func newManager(ctx context.Context, client openshell.Client) *openshell.Manager {
	return sessionctl.NewManager(ctx, client)
}

// runSession starts (req.Create) or resumes the session, then attaches the
// SSH terminal to it unless detach is set.
func runSession(cmd *cobra.Command, req sessionctl.Request, detach bool) error {
	ctx := cmd.Context()
	cfg := config.FromContext(ctx)
	sess := sshutils.SessionFromContext(ctx)

	client, err := newOpenShellClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck

	req.Progress = cmd.ErrOrStderr()
	tty := false
	if sess != nil {
		if pty, _, ok := sess.Pty(); ok {
			req.Term, tty = pty.Term, true
		}
	}

	p, err := sessionctl.Prepare(ctx, newManager(ctx, client), req)
	var conflict *openshell.RestoreConflictError
	if errors.As(err, &conflict) && tty {
		// The new sandbox was deleted again; ask, and start it once more
		// with the answer.
		yes, aerr := confirm(cmd, conflict.Question())
		if aerr != nil {
			return aerr
		}
		if !yes {
			return err
		}
		req.Restore = openshell.RestoreOver
		p, err = sessionctl.Prepare(ctx, newManager(ctx, client), req)
	}
	if err != nil {
		return err
	}
	defer p.Close()

	if detach {
		s := p.Session
		if s.Restored != nil {
			cmd.PrintErrf("Restored /sandbox from %s (%s).\n", s.Restored.URL, s.Restored.Describe())
		}
		cmd.PrintErrf("Session %s (%s) is running in sandbox %s.\n", s.Name, strings.Join(s.Repos, ", "), s.Sandbox)
		cmd.PrintErrf("Attach with: session attach %s\n", s.Name)
		return nil
	}

	return attach(ctx, p, sess)
}

// errCancelled is returned when the user cancels a question with Ctrl-C.
var errCancelled = errors.New("cancelled")

// confirm asks a yes/no question on the SSH terminal and reads the answer
// line, echoing it: y or yes is yes, anything else no. Ctrl-C cancels; the
// end of the input is no.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	cmd.PrintErr(question + " [y/N] ")
	in := cmd.InOrStdin()
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n == 1 {
			switch c := buf[0]; {
			case c == '\r' || c == '\n':
				cmd.PrintErr("\r\n")
				answer := strings.ToLower(strings.TrimSpace(string(line)))
				return answer == "y" || answer == "yes", nil
			case c == 3: // Ctrl-C
				cmd.PrintErr("^C\r\n")
				return false, errCancelled
			case c == 4: // Ctrl-D
				cmd.PrintErr("\r\n")
				return false, nil
			case c == 127 || c == 8:
				if len(line) > 0 {
					line = line[:len(line)-1]
					cmd.PrintErr("\b \b")
				}
			case c >= 32 && c < 127 && len(line) < 16:
				line = append(line, c)
				cmd.PrintErr(string(c))
			}
		}
		if err != nil {
			cmd.PrintErr("\r\n")
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err //nolint:wrapcheck
		}
	}
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

// sessionRow is one session of `session ls`.
type sessionRow = sessionctl.Row

// sessionColumns are the columns of `session ls`.
var sessionColumns = []struct {
	header string
	value  func(sessionRow, time.Time) string
}{
	{"Name", func(r sessionRow, _ time.Time) string { return r.Info.Name }},
	{"Repositories", func(r sessionRow, _ time.Time) string { return strings.Join(r.Info.Repos, ", ") }},
	{"Profile", func(r sessionRow, _ time.Time) string { return orDash(r.Info.Profile) }},
	{"Sandbox", func(r sessionRow, _ time.Time) string { return r.Info.Sandbox }},
	{"Phase", func(r sessionRow, _ time.Time) string { return orDash(r.Info.Phase) }},
	{"State", func(r sessionRow, _ time.Time) string { return r.Label() }},
	{"Message", func(r sessionRow, _ time.Time) string {
		if note := r.Note(); note != "" {
			return note
		}
		return orDash(signals.RecordMessage(r.Record(), maxMessage))
	}},
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
	Name string `json:"name"`
	// Repo is the first repository; Repos are all of them.
	Repo    string   `json:"repo"`
	Repos   []string `json:"repos"`
	Profile string   `json:"profile,omitempty"`
	// Providers are the OpenShell providers attached to the session's
	// sandbox.
	Providers []string `json:"providers,omitempty"`
	Sandbox   string   `json:"sandbox"`
	Phase     string   `json:"phase"`
	Parked    bool     `json:"parked"`
	// Park is how a parked session was parked: "memory" (its sandbox
	// suspended, its program frozen) or "snapshot" (its sandbox deleted
	// after /sandbox was saved to S3; its program was stopped and
	// restarts on attach). Empty when it is not parked.
	Park string `json:"park,omitempty"`
	// SnapshotSize is the size of the snapshot of a session parked to
	// it.
	SnapshotSize int64  `json:"snapshot_size,omitempty"`
	State        string `json:"state"`
	Kind         string `json:"kind,omitempty"`
	Progress     *int   `json:"progress,omitempty"`
	App          string `json:"app,omitempty"`
	Title        string `json:"title,omitempty"`
	Msg          string `json:"msg,omitempty"`
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
		Repo:      r.Info.Repo,
		Repos:     r.Info.Repos,
		Name:      r.Info.Name,
		Profile:   r.Info.Profile,
		Providers: r.Info.Providers,
		Sandbox:   r.Info.Sandbox,
		Phase:     r.Info.Phase,
		Parked:    r.Parked(),
		State:     string(rec.State),
		Kind:      signals.Sanitize(string(rec.Kind), 0),
		Progress:  rec.Progress,
		App:       signals.Sanitize(rec.App, 0),
		Title:     signals.Sanitize(rec.Title, 0),
		Msg:       signals.Sanitize(rec.Msg, 0),
	}
	if j.State == "" {
		j.State = "none"
	}
	switch {
	case r.ParkedToSnapshot():
		j.Park, j.SnapshotSize = "snapshot", r.Info.Snapshot.Size
	case j.Parked:
		j.Park = "memory"
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
		Long: `List your agent sessions: name, repositories, profile, sandbox, phase,
and the agent's status (OSC 7501) as Sundae last read it: state, message,
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
		Use:   "park SESSION",
		Short: "Park (suspend) an agent session now",
		Long: `Park an agent session: suspend its sandbox, keeping its memory where the
OpenShell compute driver supports it, so it uses no CPU until it is resumed.
"session attach" and "session resume" resume it.

A session with an open bridge is not parked; detach first. Sessions are also
parked automatically after sessions.suspend.after with nobody attached and
no working status.

Where the gateway cannot suspend and sessions.suspend.fallback is snapshot,
the session is parked to an S3 snapshot instead: /sandbox is saved and the
sandbox deleted. That stops its program; resuming recreates the sandbox,
restores /sandbox and starts the program again.

` + sessionArgHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			n, err := resolveSession(cmd, args[0], name)
			if err != nil {
				return err
			}
			client, err := newOpenShellClient(config.FromContext(ctx))
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			parked, toSnapshot, err := newManager(ctx, client).ParkSession(ctx, proto.UserFromContext(ctx), n)
			if err != nil {
				return err
			}
			switch {
			case parked && toSnapshot:
				cmd.PrintErrf("Session %s parked to an S3 snapshot: its sandbox was deleted and its program stopped. Resume it with: session resume %s\n", n, n)
			case parked:
				cmd.PrintErrf("Session %s parked. Resume it with: session resume %s\n", n, n)
			default:
				cmd.PrintErrf("Session %s is already parked\n", n)
			}
			return nil
		},
	}
	addLegacyNameFlag(cmd, &name)
	return cmd
}

func sessionResumeCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "resume SESSION",
		Short: "Resume a parked agent session without attaching",
		Long: `Resume an agent session without attaching to it: resume its sandbox if it
is parked, renew its git credentials, clone repositories it lacks, and start
its program again if it is not running. It never creates a session.

` + sessionArgHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := resolveSession(cmd, args[0], name)
			if err != nil {
				return err
			}
			return runSession(cmd, sessionctl.Request{Name: n}, true)
		},
	}
	addLegacyNameFlag(cmd, &name)
	return cmd
}

func sessionEndCommand() *cobra.Command {
	var name string
	var force bool
	cmd := &cobra.Command{
		Use:     "end SESSION",
		Aliases: []string{"stop", "rm"},
		Short:   "End an agent session, deleting its sandbox",
		Long: `End an agent session, deleting its sandbox and revoking its git
credentials.

With S3 snapshots on end (snapshots.s3.on), /sandbox is saved to S3 first,
replacing the session's previous snapshot, and starting the session again
restores it. When the snapshot fails the session is not ended; --force ends
it anyway, without a snapshot. A session parked to its snapshot is ended by
forgetting that it is parked; its snapshot is kept.

` + sessionArgHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			n, err := resolveSession(cmd, args[0], name)
			if errors.Is(err, openshell.ErrNoSession) {
				cmd.PrintErrln("No session to end")
				return nil
			}
			if err != nil {
				return err
			}
			client, err := newOpenShellClient(config.FromContext(ctx))
			if err != nil {
				return err
			}
			defer client.Close() //nolint:errcheck

			res, err := newManager(ctx, client).EndSession(ctx, proto.UserFromContext(ctx), n, openshell.EndOptions{
				Force:    force,
				Progress: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			switch {
			case res.Parked:
				cmd.PrintErrln("Session ended; its S3 snapshot is kept")
			case res.Existed && res.Snapshot != nil:
				cmd.PrintErrf("Session ended; /sandbox is saved in %s (%s)\n", res.Snapshot.URL, res.Snapshot.Describe())
			case res.Existed:
				cmd.PrintErrln("Session ended")
			default:
				cmd.PrintErrln("No session to end")
			}
			return nil
		},
	}

	addLegacyNameFlag(cmd, &name)
	cmd.Flags().BoolVarP(&force, "force", "f", false, "End the session even when its snapshot fails")

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
