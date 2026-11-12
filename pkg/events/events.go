// Package events is Soft Serve's in-process event bus: session status
// changes, session lifecycle and forge outcomes are published once and
// consumed by any number of subscribers (notifications, the TUI, a CLI feed).
//
// The bus lives for the server's lifetime. It is created in server setup and
// reaches SSH sessions and jobs through the context (FromContext). Publishing
// never blocks: each subscriber has a bounded buffer and a subscriber that
// falls behind loses events, which are counted.
package events

import "time"

// Kind is the type of an event. It says which payload field is set.
type Kind string

const (
	// KindSessionStatus is a change in a session's OSC 7501 status. Status
	// is set.
	KindSessionStatus Kind = "session.status"
	// KindSessionLifecycle is a session lifecycle step: created, ready,
	// resumed, parked, ended or failed. Lifecycle is set.
	KindSessionLifecycle Kind = "session.lifecycle"
	// KindForge is the outcome of a forge operation. Forge is set.
	KindForge Kind = "forge"
)

// Event is one thing that happened. Exactly one payload field is set, as
// named by Kind.
type Event struct {
	// ID is assigned by the bus when the event is published. IDs are
	// unique for the life of the process (they start again at 1 after a
	// restart) and increase with each publish. Events published
	// concurrently may reach a subscriber slightly out of ID order.
	ID uint64 `json:"id"`
	// Time is when the event happened. The bus sets it when it is zero.
	Time time.Time `json:"time"`
	// UserID is the ID of the user the event belongs to: the session's
	// owner, or the user who ran a forge command. 0 means no user, e.g. a
	// scheduled forge sync.
	UserID int64 `json:"user_id,omitempty"`
	// Kind names the payload.
	Kind Kind `json:"kind"`

	Status    *SessionStatus    `json:"status,omitempty"`
	Lifecycle *SessionLifecycle `json:"lifecycle,omitempty"`
	Forge     *ForgeOutcome     `json:"forge,omitempty"`
}

// payloadKind returns the Kind matching the payload that is set, or "" when
// none is.
func (e Event) payloadKind() Kind {
	switch {
	case e.Status != nil:
		return KindSessionStatus
	case e.Lifecycle != nil:
		return KindSessionLifecycle
	case e.Forge != nil:
		return KindForge
	default:
		return ""
	}
}

// Session identifies an agent session.
type Session struct {
	// User is the owner's username.
	User string `json:"user"`
	// Repo is the repository the session works on.
	Repo string `json:"repo"`
	// Name is the session's own name. Empty while there is one unnamed
	// session per (user, repository).
	Name string `json:"name,omitempty"`
	// Sandbox is the OpenShell sandbox name.
	Sandbox string `json:"sandbox"`
}

// DisplayName is the name to show for the session: Name, or the sandbox
// name when the session has no name of its own.
func (s Session) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Sandbox
}

// State is an OSC 7501 program state.
type State string

// OSC 7501 states. StateNone is not part of the protocol: it is a session
// with no record, because the program never reported, cleared its record, or
// its working or blocked record was dropped at process exit or the next
// shell prompt.
const (
	StateNone    State = ""
	StateIdle    State = "idle"
	StateWorking State = "working"
	StateDone    State = "done"
	StateBlocked State = "blocked"
	StateError   State = "error"
)

// States lists the OSC 7501 states, not counting StateNone.
var States = []State{StateIdle, StateWorking, StateDone, StateBlocked, StateError}

// Glyph is the status glyph used everywhere a state is shown (TUI, session
// ls, notification titles).
func (s State) Glyph() string {
	switch s {
	case StateBlocked:
		return "▲"
	case StateError:
		return "✗"
	case StateDone:
		return "✓"
	case StateWorking:
		return "●"
	case StateIdle:
		return "○"
	default:
		return "·"
	}
}

// BlockKind says what a blocked program is waiting for (OSC 7501 kind).
type BlockKind string

// OSC 7501 kinds. Only meaningful with StateBlocked.
const (
	BlockPermission BlockKind = "permission"
	BlockQuestion   BlockKind = "question"
	BlockAuth       BlockKind = "auth"
)

// Record is one OSC 7501 record, with title and msg already base64-decoded.
// Title and Msg are untrusted program output: whoever shows them outside a
// terminal must sanitise them.
type Record struct {
	// ID is the record id ("build/test"); empty for the root record.
	ID       string    `json:"id,omitempty"`
	State    State     `json:"state"`
	Kind     BlockKind `json:"kind,omitempty"`
	Progress *int      `json:"progress,omitempty"`
	App      string    `json:"app,omitempty"`
	Title    string    `json:"title,omitempty"`
	Msg      string    `json:"msg,omitempty"`
}

// SessionStatus is a change in one record of a session's status.
type SessionStatus struct {
	Session Session `json:"session"`
	// Record is the record as it is now. Record.State is StateNone when the
	// record is gone.
	Record Record `json:"record"`
	// Previous is the record's state before this change.
	Previous State `json:"previous"`
}

// Lifecycle is a session lifecycle step.
type Lifecycle string

// Lifecycle steps.
const (
	// LifecycleCreated: the sandbox was created.
	LifecycleCreated Lifecycle = "created"
	// LifecycleResumed: an existing sandbox was reused (started again if it
	// was stopped).
	LifecycleResumed Lifecycle = "resumed"
	// LifecycleReady: the session is ready to attach to.
	LifecycleReady Lifecycle = "ready"
	// LifecycleParked: the sandbox was suspended while idle.
	LifecycleParked Lifecycle = "parked"
	// LifecycleEnded: the session was ended and its sandbox deleted.
	LifecycleEnded Lifecycle = "ended"
	// LifecycleFailed: starting or ending the session failed. Reason says
	// why.
	LifecycleFailed Lifecycle = "failed"
)

// SessionLifecycle is a lifecycle step of a session.
type SessionLifecycle struct {
	Session Session   `json:"session"`
	Step    Lifecycle `json:"step"`
	// Op is the operation that failed ("start", "end"); set for
	// LifecycleFailed.
	Op string `json:"op,omitempty"`
	// Reason says why a step failed. It may quote command output.
	Reason string `json:"reason,omitempty"`
}

// ForgeOutcome is the outcome of one forge operation.
type ForgeOutcome struct {
	// Kind is the operation: import, sync, push or discover.
	Kind  string `json:"kind"`
	Forge string `json:"forge"`
	// Repo is the Soft Serve repository; Path the forge path. Both are
	// empty for discover.
	Repo string `json:"repo,omitempty"`
	Path string `json:"path,omitempty"`
	OK   bool   `json:"ok"`
	// Error is set when OK is false.
	Error string `json:"error,omitempty"`
}

// SessionStatusEvent returns a session status event owned by userID.
func SessionStatusEvent(userID int64, s SessionStatus) Event {
	return Event{UserID: userID, Kind: KindSessionStatus, Status: &s}
}

// SessionLifecycleEvent returns a session lifecycle event owned by userID.
func SessionLifecycleEvent(userID int64, l SessionLifecycle) Event {
	return Event{UserID: userID, Kind: KindSessionLifecycle, Lifecycle: &l}
}

// ForgeEvent returns a forge event; userID is 0 when no user ran it.
func ForgeEvent(userID int64, f ForgeOutcome) Event {
	return Event{UserID: userID, Kind: KindForge, Forge: &f}
}
