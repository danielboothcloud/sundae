package openshell

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
)

// Monitor polling bounds.
const (
	// DefaultPollConcurrency is how many sandboxes a poll execs in at once.
	DefaultPollConcurrency = 8
	// DefaultPollTimeout bounds one gateway call of a poll: the sandbox
	// listing, and each sandbox's `osewa local ls` exec.
	DefaultPollTimeout = 15 * time.Second
	// parkTimeout bounds one park: the suspend call and the wait for the
	// sandbox to be Suspended.
	parkTimeout = readyTimeout
)

// Who parked a session: the Op of a LifecycleParked event.
const (
	// ParkAuto: the Monitor parked an idle session.
	ParkAuto = "auto"
	// ParkManual: the user ran `session park`.
	ParkManual = "park"
)

// ErrBridgeOpen is returned when parking a session that has an open SSH
// bridge.
var ErrBridgeOpen = errors.New("the session has an open bridge; detach first")

// ErrParkInProgress is returned when the session is already being parked.
var ErrParkInProgress = errors.New("the session is already being parked")

// SessionStatus is what the Monitor knows of one session.
type SessionStatus struct {
	// Session identifies the session.
	Session events.Session `json:"session"`
	// UserID is the owner's user ID.
	UserID int64 `json:"user_id"`
	// Profile is the profile the session was created from; empty for none.
	Profile string `json:"profile,omitempty"`
	// Phase is the sandbox phase at the last listing.
	Phase string `json:"phase"`
	// Parked reports whether the sandbox is suspended (or suspending). A
	// parked session keeps the status it had when it was parked.
	Parked bool `json:"parked"`
	// Record is the session's latest root OSC 7501 record; its State is
	// events.StateNone when there is none. Title and Msg are untrusted
	// program output.
	Record events.Record `json:"record"`
	// OsewaID is the id of the osewa session recorded in the sandbox; empty
	// when none was started yet.
	OsewaID string `json:"osewa_id,omitempty"`
	// Running reports whether the recorded osewa session exists and its
	// program has not exited.
	Running bool `json:"running"`
	// Exited reports whether the program exited; ExitCode is its status.
	Exited   bool `json:"exited"`
	ExitCode *int `json:"exit_code,omitempty"`
	// Attached is the number of osewa clients attached, Sundae's own
	// bridges included.
	Attached int `json:"attached"`
	// IdleSeconds is how long osewa has seen no attached client and no
	// output.
	IdleSeconds int64 `json:"idle_seconds"`
	// Bridges is the number of SSH bridges Sundae has open on the
	// session, counting start and attach commands still preparing it.
	Bridges int `json:"bridges"`
	// Changed is when Record last changed, as far as Sundae has seen:
	// at most a poll interval late, and the first poll after Sundae
	// started for status that predates it. Zero before the first poll.
	Changed time.Time `json:"changed"`
	// Polled is when the status was last read successfully.
	Polled time.Time `json:"polled"`
	// Error is why the last poll failed; empty when it succeeded.
	Error string `json:"error,omitempty"`
	// Stale reports that the status is the last one known: the last poll
	// failed, or the sandbox is neither running nor parked.
	Stale bool `json:"stale"`
	// Unsuspendable is why the gateway refused to suspend the sandbox;
	// Sundae does not try again while it runs.
	Unsuspendable string `json:"unsuspendable,omitempty"`
}

// entry is the Monitor's state of one sandbox.
type entry struct {
	st SessionStatus
	// known reports whether a poll has succeeded.
	known bool
	// seed marks sandboxes of the Monitor's first listing: their first
	// poll sets a baseline without publishing, so a restart does not
	// replay every session's status.
	seed bool
	// lastActive is the last time the session was seen attached, bridged,
	// working, or (re)started, or the first time it was seen at all.
	lastActive time.Time
	// failing reports that polls are failing, so that is logged once.
	failing bool
}

// Monitor follows every session Sundae manages. On an interval it lists
// the sandboxes Sundae created and, in each Ready one, runs `osewa local
// ls` (one exec each, a bounded number at a time) to read the session's
// status, publishing changes on the event bus. It counts the SSH bridges
// open on each session and, when parking is enabled, suspends sessions that
// nobody has used for a while.
//
// It keeps everything in memory; after a restart it starts again from what
// the sandboxes report. A nil *Monitor is valid: it knows nothing and
// tracks nothing.
type Monitor struct {
	newClient   func() (Client, error)
	bus         *events.Bus
	logger      *log.Logger
	now         func() time.Time
	tick        func(time.Duration) <-chan time.Time
	interval    time.Duration
	timeout     time.Duration
	concurrency int
	suspend     config.SessionSuspendConfig
	// snaps is the S3 snapshot store; nil when snapshots are off.
	snaps *snapshotStore
	// fallback parks sessions the gateway cannot suspend to their S3
	// snapshot (sessions.suspend.fallback: snapshot).
	fallback bool

	mu      sync.Mutex
	entries map[string]*entry
	// bridges counts the open bridges per sandbox.
	bridges map[string]int
	// parking holds a channel per sandbox being parked, closed when done.
	parking map[string]chan struct{}
	// unsuspendable maps a sandbox to the gateway's refusal to suspend it.
	unsuspendable map[string]string
	// listed reports whether a listing has succeeded.
	listed bool
	// warnedUnsupported reports whether a refused suspend was logged.
	warnedUnsupported bool
	// listFailing reports that listings are failing, so that is logged once.
	listFailing bool

	// parks tracks background parks; parkSem bounds them.
	parks   sync.WaitGroup
	parkSem chan struct{}
}

// MonitorOption configures a Monitor.
type MonitorOption func(*Monitor)

// WithMonitorEvents publishes status changes, exits and parks on bus.
func WithMonitorEvents(bus *events.Bus) MonitorOption {
	return func(m *Monitor) { m.bus = bus }
}

// WithMonitorLogger sets the logger.
func WithMonitorLogger(l *log.Logger) MonitorOption {
	return func(m *Monitor) { m.logger = l }
}

// NewMonitor returns a Monitor configured from cfg.Sessions. newClient is
// called for every poll and every park, and the client closed after, so a
// rotated gateway token is picked up as it is by the session commands.
func NewMonitor(newClient func() (Client, error), cfg *config.Config, opts ...MonitorOption) *Monitor {
	m := &Monitor{
		newClient:     newClient,
		now:           time.Now,
		tick:          time.After,
		interval:      cfg.Sessions.Status.PollInterval,
		timeout:       DefaultPollTimeout,
		concurrency:   DefaultPollConcurrency,
		suspend:       cfg.Sessions.Suspend,
		snaps:         newSnapshotStore(cfg),
		fallback:      cfg.SnapshotFallback(),
		entries:       map[string]*entry{},
		bridges:       map[string]int{},
		parking:       map[string]chan struct{}{},
		unsuspendable: map[string]string{},
	}
	if m.interval <= 0 {
		m.interval = config.DefaultStatusPollInterval
	}
	if m.suspend.After <= 0 {
		m.suspend.After = config.DefaultSuspendAfter
	}
	for _, o := range opts {
		o(m)
	}
	if m.logger == nil {
		m.logger = log.Default()
	}
	m.logger = m.logger.WithPrefix("sessions")
	m.parkSem = make(chan struct{}, m.concurrency)
	return m
}

// Run polls at once and then every poll interval until ctx is done, and
// returns once the parks it started have finished.
func (m *Monitor) Run(ctx context.Context) {
	if m == nil {
		return
	}
	defer m.parks.Wait()
	for {
		m.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.tick(m.interval):
		}
	}
}

// Poll runs one round now: list the sandboxes, read every running
// session's status, and start parking the sessions that are due. Run calls
// it on every tick; parks it starts finish in the background.
func (m *Monitor) Poll(ctx context.Context) {
	if m == nil {
		return
	}
	client, err := m.newClient()
	if err != nil {
		m.listFailed(err)
		return
	}
	defer client.Close() //nolint:errcheck

	lctx, cancel := context.WithTimeout(ctx, m.timeout)
	sbs, err := client.ListSandboxes(lctx, map[string]string{LabelManaged: "true"})
	cancel()
	if err != nil {
		m.listFailed(err)
		return
	}

	m.pollAll(ctx, client, m.reconcile(sbs))
	if m.suspend.Enabled {
		m.parkIdle(ctx)
	}
}

// listFailed marks every session stale after a failed listing.
func (m *Monitor) listFailed(err error) {
	m.mu.Lock()
	for _, e := range m.entries {
		e.st.Stale = true
		e.st.Error = err.Error()
	}
	first := !m.listFailing
	m.listFailing = true
	m.mu.Unlock()
	if first {
		m.logger.Warn("cannot list session sandboxes; keeping the last known status", "err", err)
	}
}

// managedSandbox returns the owner of a sandbox Sundae created: one
// labelled managed, with a numeric owner and a repository. Sundae never
// polls or parks any other.
func managedSandbox(sb *Sandbox) (userID int64, ok bool) {
	if sb == nil || sb.Labels[LabelManaged] != "true" || sb.Annotations[AnnotationRepo] == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(sb.Labels[LabelUserID], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// sessionOf returns the session a sandbox holds.
func sessionOf(sb *Sandbox) events.Session {
	name := sb.Annotations[AnnotationSession]
	if name == "" {
		name = DefaultSessionName
	}
	return events.Session{
		User:    sb.Annotations[AnnotationUser],
		Repo:    sb.Annotations[AnnotationRepo],
		Name:    name,
		Sandbox: sb.Name,
	}
}

func isParkedPhase(p v1.SandboxPhase) bool {
	return p == v1.SandboxSuspended || p == v1.SandboxSuspending
}

// Sandbox lifecycle words, shown after a session's state.
const (
	WordParked   = "parked"
	WordStarting = "starting"
	// WordParkedSnapshot is a session parked to its S3 snapshot: it has no
	// sandbox, and its program was stopped.
	WordParkedSnapshot = "parked (snapshot)"
)

// PhaseSnapshot is the phase Sundae shows for a session parked to its
// S3 snapshot. It is not a sandbox phase: the session has no sandbox.
const PhaseSnapshot = "Snapshot"

// PhaseWord returns the lifecycle word shown after a session's state for a
// sandbox phase: WordParked for a suspended (or suspending) sandbox,
// WordParkedSnapshot for PhaseSnapshot, WordStarting for one being
// provisioned or started, else "".
func PhaseWord(phase string) string {
	switch v1.SandboxPhase(phase) {
	case v1.SandboxSuspended, v1.SandboxSuspending:
		return WordParked
	case v1.SandboxProvisioning, v1.SandboxStarting:
		return WordStarting
	case PhaseSnapshot:
		return WordParkedSnapshot
	}
	return ""
}

// IsParkedWord reports whether a lifecycle word is a park: WordParked or
// WordParkedSnapshot.
func IsParkedWord(word string) bool {
	return word == WordParked || word == WordParkedSnapshot
}

// reconcile brings the entries in line with a listing and returns the
// sandboxes to poll: the Ready ones.
func (m *Monitor) reconcile(sbs []*Sandbox) []string {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.listFailing {
		m.logger.Info("listing session sandboxes again")
	}
	m.listFailing = false
	seed := !m.listed
	m.listed = true

	seen := map[string]bool{}
	var ready []string
	for _, sb := range sbs {
		userID, ok := managedSandbox(sb)
		if !ok || seen[sb.Name] {
			continue
		}
		seen[sb.Name] = true
		e := m.entries[sb.Name]
		if e == nil {
			e = &entry{seed: seed, lastActive: now}
			m.entries[sb.Name] = e
		}
		e.st.Session = sessionOf(sb)
		e.st.UserID = userID
		e.st.Profile = sb.Annotations[AnnotationProfile]

		prev := e.st.Phase
		phase := sb.Status.Phase
		e.st.Phase = string(phase)
		e.st.Parked = isParkedPhase(phase)
		if phase == v1.SandboxReady {
			if prev != "" && prev != string(v1.SandboxReady) {
				// Resumed or started again: a new idle window.
				e.lastActive = now
			}
			ready = append(ready, sb.Name)
			continue
		}
		// Not exec'd: keep the last known status. A parked session's
		// program is frozen, so its status still holds.
		e.st.Stale = !e.st.Parked
		if !e.st.Parked {
			e.st.Error = ""
		}
	}
	for name := range m.entries {
		if !seen[name] {
			delete(m.entries, name)
			delete(m.unsuspendable, name)
		}
	}
	slices.Sort(ready)
	return ready
}

// pollAll polls the sandboxes, at most m.concurrency at a time.
func (m *Monitor) pollAll(ctx context.Context, client Client, sandboxes []string) {
	sem := make(chan struct{}, m.concurrency)
	var wg sync.WaitGroup
loop:
	for _, name := range sandboxes {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			m.pollSandbox(ctx, client, name)
		}()
	}
	wg.Wait()
}

// pollSandbox reads one sandbox's session status.
func (m *Monitor) pollSandbox(ctx context.Context, client Client, sandbox string) {
	cctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	out, err := runScript(cctx, client, sandbox, osewaStatusName, osewaStatusScript, nil, osewaPath(), osewaSessionFile)
	var (
		id string
		ls osewaList
	)
	if err == nil {
		var list string
		id, list, _ = strings.Cut(out, "\n")
		id = strings.TrimSpace(id)
		if id != "" {
			ls, err = parseOsewaList(list)
		}
	}
	if ctx.Err() != nil {
		// Shutting down: nothing was learnt.
		return
	}
	m.observe(sandbox, id, ls, err)
}

// recordOf converts an osewa status into an event record.
func recordOf(st *osewaProgramStatus) events.Record {
	if st == nil || st.State == "" || st.State == "clear" {
		return events.Record{}
	}
	r := events.Record{
		State: events.State(st.State),
		Kind:  events.BlockKind(st.Kind),
		App:   st.App,
		Title: st.Title,
		Msg:   st.Msg,
	}
	if st.Progress != nil && *st.Progress >= 0 && *st.Progress <= 100 {
		p := int(*st.Progress)
		r.Progress = &p
	}
	return r
}

// sameRecord reports whether two records are equal.
func sameRecord(a, b events.Record) bool {
	pa, pb := -1, -1
	if a.Progress != nil {
		pa = *a.Progress
	}
	if b.Progress != nil {
		pb = *b.Progress
	}
	return a.ID == b.ID && a.State == b.State && a.Kind == b.Kind && pa == pb &&
		a.App == b.App && a.Title == b.Title && a.Msg == b.Msg
}

// observe records a poll's result and publishes what changed.
func (m *Monitor) observe(sandbox, id string, ls osewaList, pollErr error) {
	now := m.now()
	m.mu.Lock()
	e := m.entries[sandbox]
	if e == nil {
		// Ended while it was polled.
		m.mu.Unlock()
		return
	}
	if pollErr != nil {
		e.st.Error = pollErr.Error()
		e.st.Stale = true
		first := !e.failing
		e.failing = true
		m.mu.Unlock()
		if first {
			m.logger.Warn("cannot read session status; keeping the last known", "sandbox", sandbox, "err", pollErr)
		}
		return
	}
	if e.failing {
		m.logger.Info("reading session status again", "sandbox", sandbox)
	}
	e.failing = false

	s, found := ls.find(id)
	var rec events.Record
	if found {
		rec = recordOf(s.Status)
	}
	exited := found && s.Exited

	var evs []events.Event
	if e.known || !e.seed {
		if !sameRecord(e.st.Record, rec) {
			evs = append(evs, events.SessionStatusEvent(e.st.UserID, events.SessionStatus{
				Session:  e.st.Session,
				Record:   rec,
				Previous: e.st.Record.State,
			}))
		}
		sameProgram := e.known && e.st.OsewaID == id
		switch {
		case exited && !(sameProgram && e.st.Exited):
			code := int(s.ExitCode)
			evs = append(evs, events.SessionLifecycleEvent(e.st.UserID, events.SessionLifecycle{
				Session:  e.st.Session,
				Step:     events.LifecycleExited,
				ExitCode: &code,
			}))
		case !found && sameProgram && e.st.Running:
			// The program's osewa session is gone (osewa reaped it, or
			// the sandbox restarted): it exited, with no status left.
			evs = append(evs, events.SessionLifecycleEvent(e.st.UserID, events.SessionLifecycle{
				Session: e.st.Session,
				Step:    events.LifecycleExited,
			}))
		}
	}

	if !e.known || !sameRecord(e.st.Record, rec) {
		e.st.Changed = now
	}
	e.st.Record = rec
	e.st.OsewaID = id
	e.st.Running = found && !s.Exited
	e.st.Exited = exited
	e.st.ExitCode = nil
	if exited {
		code := int(s.ExitCode)
		e.st.ExitCode = &code
	}
	e.st.Attached = int(s.Attached)
	e.st.IdleSeconds = s.IdleSeconds
	e.st.Polled = now
	e.st.Error = ""
	e.st.Stale = false
	e.known = true
	if e.st.Attached > 0 || (e.st.Running && rec.State == events.StateWorking) {
		e.lastActive = now
	}
	m.mu.Unlock()

	for _, ev := range evs {
		m.bus.Publish(ev)
	}
}

// parkable reports whether a session is due to be parked, and if not, why.
//
// A session is parked once, for the whole of `after`, nobody has been
// attached (no osewa client, no Sundae bridge) and it has not reported
// working. Every other state counts as not working: idle, done, blocked,
// error, and none. A program that never reported a status (none) must also
// have been quiet: osewa has seen no output from it for `after`, so a plain
// shell running a long build is not frozen mid-build. A program that has
// exited, or whose osewa session is gone, runs nothing: it is parked like an
// idle one, and the next start or attach starts it again either way.
func parkable(e *entry, bridges int, now time.Time, after time.Duration, fallback bool) (bool, string) {
	st := e.st
	switch {
	case st.Phase != string(v1.SandboxReady):
		return false, "the sandbox is " + st.Phase
	case st.Unsuspendable != "" && !fallback:
		return false, "the gateway cannot suspend it"
	case bridges > 0:
		return false, "a bridge is open"
	case !e.known || st.Stale:
		return false, "its status is not known"
	case st.Attached > 0:
		return false, "a client is attached"
	case st.Running && st.Record.State == events.StateWorking:
		return false, "it is working"
	case now.Sub(e.lastActive) < after:
		return false, "it was used less than " + after.String() + " ago"
	case st.Running && st.Record.State == events.StateNone && time.Duration(st.IdleSeconds)*time.Second < after:
		return false, "its program reports no status and had output less than " + after.String() + " ago"
	}
	return true, ""
}

// parkIdle starts parking every session that is due.
func (m *Monitor) parkIdle(ctx context.Context) {
	now := m.now()
	m.mu.Lock()
	var due []string
	for name, e := range m.entries {
		if _, busy := m.parking[name]; busy {
			continue
		}
		if _, no := m.unsuspendable[name]; no && !m.fallback {
			continue
		}
		if ok, _ := parkable(e, m.bridges[name], now, m.suspend.After, m.fallback); ok {
			due = append(due, name)
		}
	}
	m.mu.Unlock()
	slices.Sort(due)

	for _, name := range due {
		m.parks.Add(1)
		go func() {
			defer m.parks.Done()
			select {
			case m.parkSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-m.parkSem }()
			m.autoPark(ctx, name)
		}()
	}
}

// autoPark parks an idle session and publishes parked.
func (m *Monitor) autoPark(ctx context.Context, sandbox string) {
	client, err := m.newClient()
	if err != nil {
		m.logger.Warn("cannot park session", "sandbox", sandbox, "err", err)
		return
	}
	defer client.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(ctx, parkTimeout)
	defer cancel()
	snapshot, err := m.park(ctx, client, sandbox, true)
	switch {
	case err == nil:
		m.mu.Lock()
		var (
			sess   events.Session
			userID int64
		)
		if e := m.entries[sandbox]; e != nil {
			sess, userID = e.st.Session, e.st.UserID
		}
		m.mu.Unlock()
		if userID != 0 {
			m.bus.Publish(events.SessionLifecycleEvent(userID, events.SessionLifecycle{
				Session:  sess,
				Step:     events.LifecycleParked,
				Op:       ParkAuto,
				Snapshot: snapshot,
			}))
		}
		if snapshot {
			m.logger.Info("parked idle session to an S3 snapshot and deleted its sandbox", "sandbox", sandbox, "after", m.suspend.After)
		} else {
			m.logger.Info("parked idle session", "sandbox", sandbox, "after", m.suspend.After)
		}
	case errors.Is(err, errNotDue), errors.Is(err, ErrBridgeOpen), errors.Is(err, ErrParkInProgress):
	case IsSuspendUnsupported(err):
		// Logged by park.
	default:
		if m.fallback {
			// Not again until the session has been idle for another
			// `after`.
			m.mu.Lock()
			m.touch(sandbox)
			m.mu.Unlock()
		}
		m.logger.Warn("cannot park session", "sandbox", sandbox, "err", err)
	}
}

// errNotDue is returned by park when an automatic park is no longer due.
var errNotDue = errors.New("not due")

// park parks the sandbox unless a bridge is open on it, it is being parked
// already, or the gateway has refused to suspend it before (and there is
// no snapshot fallback). auto parks re-check that the session is still
// due. Commands that open a bridge meanwhile wait for the park to finish
// (see OpenBridge), then resume it.
//
// It suspends the sandbox, after a backup snapshot when snapshots.s3.on
// includes park. Where the gateway cannot suspend and
// sessions.suspend.fallback is snapshot, it parks the session to its S3
// snapshot instead (see parkToSnapshot), and reports snapshot.
func (m *Monitor) park(ctx context.Context, client Client, sandbox string, auto bool) (snapshot bool, err error) {
	m.mu.Lock()
	if m.bridges[sandbox] > 0 {
		m.mu.Unlock()
		return false, ErrBridgeOpen
	}
	if _, busy := m.parking[sandbox]; busy {
		m.mu.Unlock()
		return false, ErrParkInProgress
	}
	reason, unsupported := m.unsuspendable[sandbox]
	if unsupported && !m.fallback {
		m.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ErrSuspendUnsupported, reason)
	}
	// The gateway has one compute driver: once it refused one sandbox,
	// the fallback does not ask again for the next.
	unsupported = unsupported || (m.fallback && m.warnedUnsupported)
	var rec events.Record
	if e := m.entries[sandbox]; e != nil {
		rec = e.st.Record
	}
	if auto {
		e := m.entries[sandbox]
		if e == nil {
			m.mu.Unlock()
			return false, errNotDue
		}
		if ok, _ := parkable(e, 0, m.now(), m.suspend.After, m.fallback); !ok {
			m.mu.Unlock()
			return false, errNotDue
		}
	}
	done := make(chan struct{})
	m.parking[sandbox] = done
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.parking, sandbox)
		m.mu.Unlock()
		close(done)
	}()

	if m.snaps != nil {
		unlock, err := sandboxLocks.Lock(ctx, sandbox)
		if err != nil {
			return false, err
		}
		defer unlock()
	}

	op := ParkManual
	if auto {
		op = ParkAuto
	}
	if !unsupported {
		var backup *SnapshotInfo
		if m.snaps.onPark() {
			backup = m.backup(ctx, client, sandbox)
		}
		sb, err := client.SuspendSandbox(ctx, sandbox)
		switch {
		case err == nil:
			m.mu.Lock()
			if e := m.entries[sandbox]; e != nil {
				e.st.Phase = string(v1.SandboxSuspended)
				if sb != nil && sb.Status.Phase != "" {
					e.st.Phase = string(sb.Status.Phase)
				}
				e.st.Parked = true
			}
			m.mu.Unlock()
			return false, nil
		case !IsSuspendUnsupported(err):
			return false, err
		}
		m.markUnsuspendable(sandbox, err)
		if !m.fallback {
			return false, err
		}
		if err := m.parkToSnapshot(ctx, client, sandbox, op, rec, backup); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := m.parkToSnapshot(ctx, client, sandbox, op, rec, nil); err != nil {
		return false, err
	}
	return true, nil
}

// backup takes the snapshot snapshots.s3.on: [park] asks for before a
// memory-retaining park. A failure is published and logged; the park goes
// on.
func (m *Monitor) backup(ctx context.Context, client Client, sandbox string) *SnapshotInfo {
	sb, err := client.GetSandbox(ctx, sandbox)
	if err != nil {
		m.logger.Warn("cannot snapshot session before parking it", "sandbox", sandbox, "err", err)
		return nil
	}
	userID, ok := managedSandbox(sb)
	if !ok {
		return nil
	}
	sess := sessionOf(sb)
	info, err := m.snaps.take(ctx, client, userID, sess, io.Discard)
	if err != nil {
		m.logger.Warn("cannot snapshot session before parking it", "sandbox", sandbox, "err", err)
		m.bus.Publish(events.SessionLifecycleEvent(userID, events.SessionLifecycle{
			Session: sess, Step: events.LifecycleFailed, Op: "snapshot", Reason: err.Error(),
		}))
		return nil
	}
	m.bus.Publish(events.SessionLifecycleEvent(userID, events.SessionLifecycle{
		Session: sess, Step: events.LifecycleSnapshotted, Op: "park", Bytes: info.Size,
	}))
	return info
}

// parkToSnapshot parks a session the gateway cannot suspend: it snapshots
// /sandbox (unless taken is a snapshot taken a moment ago), records the
// session as parked next to the snapshot (ParkedSnapshot), and deletes the
// sandbox. The session's program is stopped; start and attach recreate the
// sandbox, restore /sandbox and start the program again. The marker is the
// session's only record while it has no sandbox, which is why it is written
// before the sandbox is deleted, and removed again if the delete fails.
func (m *Monitor) parkToSnapshot(ctx context.Context, client Client, sandbox, op string, rec events.Record, taken *SnapshotInfo) error {
	sb, err := client.GetSandbox(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err)
	}
	userID, ok := managedSandbox(sb)
	if !ok {
		return fmt.Errorf("openshell: sandbox %q was not created by Sundae; not parking it", sandbox)
	}
	if sb.Status.Phase != v1.SandboxReady {
		return fmt.Errorf("openshell: sandbox %q is %s; only a running session can be parked", sandbox, sb.Status.Phase)
	}
	sess := sessionOf(sb)
	info := taken
	if info == nil {
		info, err = m.snaps.take(ctx, client, userID, sess, io.Discard)
		if err != nil {
			m.bus.Publish(events.SessionLifecycleEvent(userID, events.SessionLifecycle{
				Session: sess, Step: events.LifecycleFailed, Op: "snapshot", Reason: err.Error(),
			}))
			return fmt.Errorf("openshell: park session to a snapshot: %w", err)
		}
		m.bus.Publish(events.SessionLifecycleEvent(userID, events.SessionLifecycle{
			Session: sess, Step: events.LifecycleSnapshotted, Op: "park", Bytes: info.Size,
		}))
	}

	marker := ParkedSnapshot{
		UserID:  userID,
		User:    sess.User,
		Repo:    sess.Repo,
		Session: sess.Name,
		Sandbox: sandbox,
		Profile: sb.Annotations[AnnotationProfile],
		Parked:  m.now().UTC(),
		Op:      op,
		Record:  rec,
		Size:    info.Size,
	}
	if sb.Spec.Template != nil {
		marker.Image = sb.Spec.Template.Image
	}
	if err := m.snaps.writeMarker(ctx, marker); err != nil {
		return fmt.Errorf("openshell: record the session as parked: %w", err)
	}
	if err := client.DeleteSandbox(ctx, sandbox); err != nil {
		if derr := m.snaps.deleteMarker(context.WithoutCancel(ctx), userID, sandbox); derr != nil {
			m.logger.Warn("cannot clear a parked marker", "sandbox", sandbox, "err", derr)
		}
		return err
	}

	m.mu.Lock()
	if e := m.entries[sandbox]; e != nil {
		e.st.Phase = PhaseSnapshot
		e.st.Parked = true
		e.st.Running = false
	}
	m.mu.Unlock()
	return nil
}

// markUnsuspendable stops parking a sandbox the gateway cannot suspend. The
// first refusal is logged as a warning; the gateway has one compute driver,
// so later ones say the same and are logged at debug level.
func (m *Monitor) markUnsuspendable(sandbox string, err error) {
	m.mu.Lock()
	m.unsuspendable[sandbox] = err.Error()
	first := !m.warnedUnsupported
	m.warnedUnsupported = true
	m.mu.Unlock()
	if first {
		m.logger.Warn("the OpenShell gateway cannot suspend sandboxes; sessions it refuses are left running", "sandbox", sandbox, "err", err)
	} else {
		m.logger.Debug("the OpenShell gateway cannot suspend sandbox", "sandbox", sandbox, "err", err)
	}
}

// Park parks a session for the user's `session park`: see park. It is the
// caller's job to check ownership and publish.
func (m *Monitor) Park(ctx context.Context, client Client, sandbox string) error {
	_, err := m.ParkSession(ctx, client, sandbox)
	return err
}

// ParkSession is Park, reporting whether the session was parked to its S3
// snapshot (its sandbox deleted, its program stopped) rather than
// suspended. A nil Monitor only suspends.
func (m *Monitor) ParkSession(ctx context.Context, client Client, sandbox string) (snapshot bool, err error) {
	if m == nil {
		_, err := client.SuspendSandbox(ctx, sandbox)
		return false, err //nolint:wrapcheck
	}
	return m.park(ctx, client, sandbox, false)
}

// OpenBridge counts a bridge open on the sandbox's session until the
// returned function is called; calling it again does nothing. A session with
// an open bridge is never parked. Session commands open it before they
// prepare the session, so a session being started or resumed is not parked
// under them, and close it, deferred, when the bridge ends, however it ends.
//
// When the session is being parked, OpenBridge waits (until ctx is done) for
// that to finish, so the caller then finds it Suspended and resumes it.
func (m *Monitor) OpenBridge(ctx context.Context, sandbox string) (closeBridge func()) {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	m.bridges[sandbox]++
	m.touch(sandbox)
	wait := m.parking[sandbox]
	m.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.bridges[sandbox]--; m.bridges[sandbox] <= 0 {
				delete(m.bridges, sandbox)
			}
			m.touch(sandbox)
		})
	}
}

// Bridges returns the number of bridges open on the sandbox's session.
func (m *Monitor) Bridges(sandbox string) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bridges[sandbox]
}

// touch starts a new idle window for the sandbox's session. Call with mu
// held.
func (m *Monitor) touch(sandbox string) {
	if e := m.entries[sandbox]; e != nil {
		e.lastActive = m.now()
	}
}

// noteReady records that a session command brought the sandbox up: it is
// no longer parked, and a new idle window starts. The status stays the last
// known until the next poll.
func (m *Monitor) noteReady(sandbox string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[sandbox]; e != nil {
		e.st.Phase = string(v1.SandboxReady)
		e.st.Parked = false
		e.lastActive = m.now()
	}
}

// snapshot returns a copy of an entry's status, with the bridge count and
// suspend refusal filled in. Call with mu held.
func (m *Monitor) snapshot(name string, e *entry) SessionStatus {
	st := e.st
	st.Bridges = m.bridges[name]
	st.Unsuspendable = m.unsuspendable[name]
	if st.ExitCode != nil {
		c := *st.ExitCode
		st.ExitCode = &c
	}
	if st.Record.Progress != nil {
		p := *st.Record.Progress
		st.Record.Progress = &p
	}
	return st
}

// filter returns the sessions keep accepts, sorted by user, repository and
// session name.
func (m *Monitor) filter(keep func(*SessionStatus) bool) []SessionStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	out := make([]SessionStatus, 0, len(m.entries))
	for name, e := range m.entries {
		if keep(&e.st) {
			out = append(out, m.snapshot(name, e))
		}
	}
	m.mu.Unlock()
	slices.SortFunc(out, func(a, b SessionStatus) int {
		return cmp.Or(
			cmp.Compare(a.UserID, b.UserID),
			strings.Compare(a.Session.Repo, b.Session.Repo),
			strings.Compare(a.Session.Name, b.Session.Name),
			strings.Compare(a.Session.Sandbox, b.Session.Sandbox),
		)
	})
	return out
}

// Status returns the status of the session in the named sandbox.
func (m *Monitor) Status(sandbox string) (SessionStatus, bool) {
	if m == nil {
		return SessionStatus{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[sandbox]
	if !ok {
		return SessionStatus{}, false
	}
	return m.snapshot(sandbox, e), true
}

// All returns every session's status.
func (m *Monitor) All() []SessionStatus {
	return m.filter(func(*SessionStatus) bool { return true })
}

// ForUser returns the statuses of the user's sessions.
func (m *Monitor) ForUser(userID int64) []SessionStatus {
	return m.filter(func(st *SessionStatus) bool { return st.UserID == userID })
}

// ForRepo returns the statuses of the user's sessions of a repository.
func (m *Monitor) ForRepo(userID int64, repo string) []SessionStatus {
	return m.filter(func(st *SessionStatus) bool { return st.UserID == userID && st.Session.Repo == repo })
}

// Session returns the status of the user's named session of a repository;
// "" is DefaultSessionName.
func (m *Monitor) Session(userID int64, repo, name string) (SessionStatus, bool) {
	if name == "" {
		name = DefaultSessionName
	}
	for _, st := range m.ForRepo(userID, repo) {
		if st.Session.Name == name {
			return st, true
		}
	}
	return SessionStatus{}, false
}

// MonitorContextKey is the key for the Monitor in a context.
var MonitorContextKey = &struct{ string }{"openshell-monitor"}

// MonitorFromContext returns the Monitor from a context, or nil when there
// is none. A nil *Monitor is safe to use.
func MonitorFromContext(ctx context.Context) *Monitor {
	if m, ok := ctx.Value(MonitorContextKey).(*Monitor); ok {
		return m
	}
	return nil
}

// ContextWithMonitor returns a new context with the Monitor attached.
func ContextWithMonitor(ctx context.Context, m *Monitor) context.Context {
	return context.WithValue(ctx, MonitorContextKey, m)
}
