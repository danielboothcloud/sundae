package cmd

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/spf13/pflag"
)

// errNoTerminal is returned when a command that attaches runs without a PTY.
var errNoTerminal = errors.New("needs a terminal")

// newOpenShellClient creates the client of the session runtimes in use for a
// command invocation. Tests may replace it.
var newOpenShellClient = func(cfg *config.Config) (openshell.Client, error) {
	return openshell.NewClient(cfg)
}

// SessionCommand returns the command that manages agent sessions. It is only
// registered when sessions are on (config.SessionsEnabled).
func SessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage agent sessions",
		// NOTE: without Args and RunE, cobra prints help for an unknown
		// subcommand (e.g. the removed park/end) and exits 0.
		Args:              cobra.NoArgs,
		RunE:              func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		PersistentPreRunE: checkIfUser,
	}

	cmd.AddCommand(
		sessionNewCommand(),
		sessionStartCommand(),
		sessionAttachCommand(),
		sessionReposCommand(),
		sessionListCommand(),
		sessionSignalsCommand(),
		sessionSuspendCommand(),
		sessionResumeCommand(),
		sessionDeleteCommand(),
		sessionSavePrivateCommand(),
	)

	return cmd
}

func sessionStartCommand() *cobra.Command {
	var name, profile, image string
	var detach bool
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
			if err := checkStartFlags(cmd, args, detach); err != nil {
				return err
			}
			return checkIfReadable(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := sessionctl.Request{Repo: args[0], Name: name, Profile: profile, Image: image, Create: true}
			return runSession(cmd, req, detach)
		},
	}
	addCreateFlags(cmd, &name, &profile, &image, &detach)
	return cmd
}

func sessionNewCommand() *cobra.Command {
	var repos []string
	var profile, image string
	var detach bool
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
			if err := checkStartFlags(cmd, args, detach); err != nil {
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
			return runSession(cmd, req, detach)
		},
	}
	cmd.Flags().StringArrayVarP(&repos, "repo", "r", nil, "Repository the session holds (repeat for several)")
	addCreateFlags(cmd, nil, &profile, &image, &detach)
	return cmd
}

// sessionCreateHelp is what start and new say about creating a session.
const sessionCreateHelp = `A new session gets its own sandbox: a Kubernetes Pod, a Docker container
or an OpenShell sandbox, by its profile's runtime, else sessions.runtime.
It runs its program (the profile's command, else openshell.command, which
every runtime uses) in an osewa session, in the checkout of its only
repository, else in /sandbox. Its profile is --profile, else the
first repository's own profile ("repo profile"), else the first
sessions.repos match among its repositories, else none; --image overrides
the image. Both only apply when the session is created.

While attached, the bottom row shows the session and the key that opens
the Sundae Sessions screen (Ctrl-] unless sessions.attach.detach_key says
otherwise); the program keeps running. ~. after a newline, or closing the
connection, also leaves it running. "session attach" re-attaches, "session
suspend" stops the sandbox and keeps its storage, and "session delete"
deletes the sandbox and its storage. With --detach, start the session
without attaching. A suspended session is resumed first.`

// addCreateFlags adds the flags start and new share. name may be nil.
func addCreateFlags(cmd *cobra.Command, name, profile, image *string, detach *bool) {
	if name != nil {
		cmd.Flags().StringVarP(name, "name", "n", "", "Session name (default: from the repository)")
	}
	cmd.Flags().StringVarP(profile, "profile", "p", "", "Profile to create the session from (ignored when it already exists)")
	cmd.Flags().StringVar(image, "image", "", "Sandbox image to use when the sandbox is created (ignored when it already exists)")
	cmd.Flags().BoolVarP(detach, "detach", "d", false, "Start the session without attaching to it")
}

// checkStartFlags requires a terminal unless detach is set.
func checkStartFlags(cmd *cobra.Command, args []string, detach bool) error {
	if detach {
		return nil
	}
	if err := checkTerminal(cmd, args); err != nil {
		return fmt.Errorf("%w (or pass --detach)", err)
	}
	return nil
}

// sessionArgHelp says what a SESSION argument names.
const sessionArgHelp = `SESSION is a session name, or a repository held by exactly one of your
sessions.`

func sessionAttachCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "attach [SESSION]",
		Short: "Attach to a running agent session (requires ssh -t)",
		Long: `Attach to an existing agent session.

The session is resumed first if it is suspended, and its program is started
again if it is not running. While attached, the bottom row shows the session
and the key that opens the Sundae Sessions screen (Ctrl-] unless
sessions.attach.detach_key says otherwise); enter there attaches again. ~.
after a newline, or closing the connection, leaves too. The program keeps
running either way.

Without SESSION, attach to your only session; with several, open the
Sessions screen to choose one.

` + sessionArgHelp,
		Args: cobra.MaximumNArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && name == "" {
				// Without a terminal, several sessions need a name.
				return nil
			}
			return checkTerminal(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var n string
			var err error
			switch {
			case len(args) > 0 || name != "":
				n, err = resolveSession(cmd, strings.Join(args, ""), name)
			default:
				var chosen bool
				n, chosen, err = defaultSession(cmd)
				if chosen {
					return nil
				}
			}
			if err != nil {
				return err
			}
			if err := checkTerminal(cmd, args); err != nil {
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
if suspended) without attaching. Removing takes them out of the session's list;
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
		s := p.Session()
		cmd.PrintErrf("Session %s (%s) is running in sandbox %s.\n", s.Name, strings.Join(s.Repos, ", "), s.Sandbox)
		cmd.PrintErrf("Attach with: session attach %s\n", s.Name)
		return nil
	}

	return attach(ctx, cmd, p, sess)
}

// SessionsScreen runs the Sundae TUI on the connection's terminal, on the
// Sessions tab with the session in sandbox selected ("" for none) and a
// notice that the session labelled label was left ("" for none), until the
// user quits. It reports false when there is no TUI to run.
type SessionsScreen func(sandbox, label string) bool

// SessionsScreenKey is the context key of the connection's SessionsScreen;
// the SSH server sets it.
var SessionsScreenKey = &struct{ string }{"sessions-screen"}

// showSessions runs the context's SessionsScreen; false without one.
func showSessions(ctx context.Context, sandbox, label string) bool {
	show, ok := ctx.Value(SessionsScreenKey).(SessionsScreen)
	return ok && show != nil && show(sandbox, label)
}

// defaultSession returns the session `session attach` without SESSION
// attaches to: the user's only one. With several, the user chooses on the
// Sessions screen (chosen), or, without a terminal, must name one.
func defaultSession(cmd *cobra.Command) (name string, chosen bool, err error) {
	ctx := cmd.Context()
	client, err := newOpenShellClient(config.FromContext(ctx))
	if err != nil {
		return "", false, err
	}
	infos, err := newManager(ctx, client).List(ctx, proto.UserFromContext(ctx))
	_ = client.Close()
	switch {
	case err != nil:
		return "", false, err //nolint:wrapcheck
	case len(infos) == 0:
		return "", false, errors.New("you have no sessions; start one with: session start REPO")
	case len(infos) == 1:
		return infos[0].Name, false, nil
	case checkTerminal(cmd, nil) == nil && showSessions(ctx, "", ""):
		return "", true, nil
	}
	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	return "", false, fmt.Errorf("you have %d sessions (%s); name one: session attach SESSION", len(infos), strings.Join(names, ", "))
}

// attach bridges the SSH session to the osewa session and propagates the
// exit code of the attach client: its program's exit code when it ended, 0
// when the user left. The home key opens the Sessions screen on the same
// connection, with the session selected.
func attach(ctx context.Context, cmd *cobra.Command, p *sessionctl.Prepared, sess sshSession) error {
	s := p.Session()
	label := signals.SessionLabel(s.Name, s.Repos)
	cfg := config.FromContext(ctx)
	if pty, _, _ := sess.Pty(); !sessionctl.ShowsStatusBar(cfg, pty.Window.Height) {
		cmd.PrintErrln(sessionctl.AttachHint(label, sessionctl.HomeKey(cfg)))
	}
	code, err := p.Attach(ctx, sess)
	if errors.Is(err, openshell.ErrHome) {
		// The Sessions screen must not keep the session from suspending.
		p.Close()
		if !showSessions(ctx, s.Sandbox, label) {
			cmd.PrintErrf("Left %s; it keeps running. Re-attach: session attach %s\n", label, s.Name)
		}
		return nil
	}
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
	{"Runtime", func(r sessionRow, _ time.Time) string { return orDash(r.Info.Runtime) }},
	{"Sandbox", func(r sessionRow, _ time.Time) string { return r.Info.Sandbox }},
	{"Phase", func(r sessionRow, _ time.Time) string { return orDash(r.Info.Phase) }},
	{"State", func(r sessionRow, _ time.Time) string { return r.Label() }},
	{"Message", func(r sessionRow, _ time.Time) string {
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
	// Runtime is the session runtime its sandbox runs in.
	Runtime string `json:"runtime,omitempty"`
	// Providers are the OpenShell providers attached to the session's
	// sandbox.
	Providers []string `json:"providers,omitempty"`
	Sandbox   string   `json:"sandbox"`
	Phase     string   `json:"phase"`
	// Suspended reports whether the session's sandbox is suspended: stopped,
	// with its storage kept.
	Suspended bool   `json:"suspended"`
	State     string `json:"state"`
	Kind      string `json:"kind,omitempty"`
	Progress  *int   `json:"progress,omitempty"`
	App       string `json:"app,omitempty"`
	Title     string `json:"title,omitempty"`
	Msg       string `json:"msg,omitempty"`
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
	// Unsuspendable is why the runtime refused to suspend the session.
	Unsuspendable string `json:"unsuspendable,omitempty"`
}

func sessionToJSON(r sessionRow) sessionJSON {
	rec := r.Record()
	j := sessionJSON{
		Repo:      r.Info.Repo,
		Repos:     r.Info.Repos,
		Name:      r.Info.Name,
		Profile:   r.Info.Profile,
		Runtime:   r.Info.Runtime,
		Providers: r.Info.Providers,
		Sandbox:   r.Info.Sandbox,
		Phase:     r.Info.Phase,
		Suspended: r.Suspended(),
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
		Long: `List your agent sessions: name, repositories, profile, runtime,
sandbox, phase, and the agent's status (OSC 7501) as Sundae last read it:
state, message, how many clients are attached, and when the state last
changed.

Sessions that need you come first: blocked, error, done, working, idle,
then those with no status. A suspended session keeps the state it had when
it was suspended. Listing never resumes a suspended session.`,
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

func sessionSuspendCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "suspend SESSION",
		Short: "Suspend an agent session: stop its sandbox, keep its storage",
		Long: `Suspend an agent session: stop its sandbox and keep its storage, so it
uses no CPU or memory until it is resumed. The pod runtime deletes the pod
and keeps its PersistentVolumeClaim; the docker runtime stops the container
and keeps its volume; the OpenShell runtime suspends the sandbox where the
gateway's compute driver supports it, and fails otherwise. Its program
stops, unless the runtime keeps the sandbox's memory.

"session attach", "session resume" and "session start" start it again on
the kept storage. "session delete" deletes the sandbox and its storage.

A session with an open bridge is not suspended; detach first. Sessions are
also suspended automatically after sessions.suspend.after with nobody
attached and no working status.

Suspending first saves the private files of forge-linked repositories (see
"session save-private"); a failed save is a warning, as the storage keeps them.

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

			suspended, err := newManager(ctx, client).Suspend(ctx, proto.UserFromContext(ctx), n, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if suspended {
				cmd.PrintErrf("Session %s suspended; its storage is kept. Resume it with: session resume %s\n", n, n)
			} else {
				cmd.PrintErrf("Session %s is already suspended\n", n)
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
		Short: "Resume a suspended agent session without attaching",
		Long: `Resume an agent session without attaching to it: resume its sandbox if it
is suspended, renew its git credentials, clone repositories it lacks, and start
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

func sessionDeleteCommand() *cobra.Command {
	var (
		name    string
		discard bool
	)
	cmd := &cobra.Command{
		Use:   "delete SESSION",
		Short: "Delete an agent session, its sandbox and its storage",
		Long: `Delete an agent session: delete its sandbox and its storage (the pod
runtime's Pod and PersistentVolumeClaim, the docker runtime's container and
volume, the OpenShell sandbox) and revoke its git credentials. Work that was
not pushed is lost.

A running session's private files of forge-linked repositories are saved
first (see "session save-private"). If that fails, nothing is deleted;
--discard-private deletes anyway and loses the unsaved private files. A
session that is not running cannot save, so it is not deleted either: resume
it and run "session save-private", or pass --discard-private.

` + sessionArgHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			n, err := resolveSession(cmd, args[0], name)
			if errors.Is(err, openshell.ErrNoSession) {
				cmd.PrintErrln("No session to delete")
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

			existed, err := newManager(ctx, client).Delete(ctx, proto.UserFromContext(ctx), n,
				openshell.DeleteOptions{DiscardPrivate: discard, Progress: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			if existed {
				cmd.PrintErrln("Session deleted")
			} else {
				cmd.PrintErrln("No session to delete")
			}
			return nil
		},
	}
	addLegacyNameFlag(cmd, &name)
	cmd.Flags().BoolVar(&discard, "discard-private", false, "delete even if the private files cannot be saved, losing them")
	return cmd
}

func sessionSavePrivateCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "save-private SESSION",
		Short: "Save the private files of a session's forge-linked repositories",
		Long: `Save the private files of a running session's forge-linked repositories:
their private paths (forge private_paths, or "repo private-paths") are
committed to refs/sundae/private in the Sundae repository, never to a branch,
so the forge never receives them. New sessions restore them. Paths the
repository tracks are skipped with a warning. Suspend and delete save too.

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

			saves, err := newManager(ctx, client).SavePrivate(ctx, proto.UserFromContext(ctx), n, cmd.ErrOrStderr())
			for _, s := range saves {
				switch s.Result {
				case openshell.PrivateSaved:
					cmd.Printf("%s: saved %s (%s)\n", s.Repo, short(s.Commit), strings.Join(s.Paths, ", "))
				case openshell.PrivateUnchanged:
					cmd.Printf("%s: unchanged %s (%s)\n", s.Repo, short(s.Commit), strings.Join(s.Paths, ", "))
				default:
					cmd.Printf("%s: nothing to save (%s)\n", s.Repo, strings.Join(s.Paths, ", "))
				}
			}
			if err != nil {
				return err
			}
			if len(saves) == 0 {
				cmd.PrintErrf("Session %s has no forge-linked repositories with private paths\n", n)
			}
			return nil
		},
	}
	addLegacyNameFlag(cmd, &name)
	return cmd
}

// checkIfUser rejects anonymous callers.
func checkIfUser(cmd *cobra.Command, _ []string) error {
	if proto.UserFromContext(cmd.Context()) == nil {
		return proto.ErrUnauthorized
	}
	return nil
}

// checkTerminal requires the SSH session to have a PTY; the error names the
// ssh -t command that runs cmd with args and the flags set.
func checkTerminal(cmd *cobra.Command, args []string) error {
	sess := sshutils.SessionFromContext(cmd.Context())
	if sess != nil {
		if _, _, ok := sess.Pty(); ok {
			return nil
		}
	}
	var publicURL string
	if cfg := config.FromContext(cmd.Context()); cfg != nil {
		publicURL = cfg.SSH.PublicURL
	}
	return fmt.Errorf("session %s %w; run: %s", cmd.Name(), errNoTerminal,
		sessionctl.TerminalCommand(publicURL, commandLine(cmd, args)...))
}

// commandLine returns the arguments that run cmd again: its path below the
// root, then args, then the flags set.
func commandLine(cmd *cobra.Command, args []string) []string {
	line := append(strings.Fields(cmd.CommandPath()), args...)
	cmd.Flags().Visit(func(f *pflag.Flag) {
		switch v := f.Value.(type) {
		case pflag.SliceValue:
			for _, s := range v.GetSlice() {
				line = append(line, "--"+f.Name, s)
			}
		default:
			if f.Value.Type() == "bool" {
				line = append(line, "--"+f.Name+"="+f.Value.String())
			} else {
				line = append(line, "--"+f.Name, f.Value.String())
			}
		}
	})
	return line
}
