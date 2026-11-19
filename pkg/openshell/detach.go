package openshell

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"

	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
)

// ErrDetached is returned by Bridge when the user detached with the prefix
// key. It is not a failure: the session's program keeps running.
var ErrDetached = errors.New("detached")

// PrefixKey is the character typed with Ctrl to start a Sundae command while
// attached (']' for Ctrl-]); the zero PrefixKey is none.
type PrefixKey byte

// DefaultPrefixKey is Ctrl-]: telnet's escape, unused by shells and editors.
const DefaultPrefixKey PrefixKey = ']'

// Enabled reports whether there is a prefix key.
func (k PrefixKey) Enabled() bool { return k != 0 }

// String names the key for people, e.g. "Ctrl-]" or "Ctrl-A"; "" for none.
func (k PrefixKey) String() string {
	if !k.Enabled() {
		return ""
	}
	return "Ctrl-" + strings.ToUpper(string(rune(k)))
}

// controlByte is the byte a terminal sends for the key in its plain encoding.
func (k PrefixKey) controlByte() byte { return byte(k) & 0x1f }

// keyCode is the key's code in the CSI-u and modifyOtherKeys encodings: the
// unshifted character.
func (k PrefixKey) keyCode() int { return int(strings.ToLower(string(rune(k)))[0]) }

// SwitchDir is the session a switch asks for.
type SwitchDir int

const (
	// SwitchNext is the next session by name (prefix n).
	SwitchNext SwitchDir = iota + 1
	// SwitchPrev is the previous session by name (prefix p).
	SwitchPrev
	// SwitchPick is a session the user chooses (prefix s).
	SwitchPick
)

// SwitchRequest is the error Bridge returns, with code 0, when the user asked
// to switch sessions. As for a detach, the session's program keeps running.
type SwitchRequest struct{ Dir SwitchDir }

func (r *SwitchRequest) Error() string {
	switch r.Dir {
	case SwitchNext:
		return "switch to the next session"
	case SwitchPrev:
		return "switch to the previous session"
	default:
		return "choose a session"
	}
}

// action is what a key after the prefix asks the bridge to do.
type action int

const (
	actionNone action = iota
	actionDetach
	actionHelp
	actionNext
	actionPrev
	actionPick
)

// switchDir returns the switch an action asks for; 0 for none.
func (a action) switchDir() SwitchDir {
	switch a {
	case actionNext:
		return SwitchNext
	case actionPrev:
		return SwitchPrev
	case actionPick:
		return SwitchPick
	default:
		return 0
	}
}

// prefixBindings maps the key typed after the prefix to its action; Bridge
// handles each action. The switch keys are bound only WithSwitching.
var prefixBindings = map[rune]action{
	'd': actionDetach,
	'?': actionHelp,
	'n': actionNext,
	'p': actionPrev,
	's': actionPick,
}

// noOtherSession is shown for prefix n or p when there is nothing to switch to.
const noOtherSession = "\r\n[sundae] no other session\r\n"

// prefixHelp is the help line shown for prefix ?.
func prefixHelp(k PrefixKey, switching bool) string {
	p := k.String()
	help := "\r\n[sundae] " + p + " d detach · "
	if switching {
		help += p + " n/p next/previous session · " + p + " s choose a session · "
	}
	return help + p + " " + p + " send " + p + " · ~. (after a newline) detach\r\n"
}

// Modifier bits of the CSI-u and modifyOtherKeys encodings (the parameter
// minus one).
const (
	modShift    = 1
	modCtrl     = 4
	modLocks    = 64 | 128 // caps lock, num lock
	escape      = 0x1b
	maxSequence = 32 // longest CSI parameter list held back waiting for its end
)

// key is one key press decoded from the input.
type key struct {
	// plain is a single byte; b is that byte.
	plain bool
	b     byte
	// code, shifted and mods are a CSI-u or modifyOtherKeys key: its code,
	// the shifted alternate (0 when not sent) and the modifier bits.
	code, shifted, mods int
	// release is a key release (kitty's report event types).
	release bool
	// csi is any other escape sequence or byte, passed through untouched.
	csi bool
}

// char is the character the key types; 0 with Ctrl, Alt or Super.
func (k key) char() rune {
	switch {
	case k.plain:
		if k.b < 0x20 || k.b >= 0x7f {
			return 0
		}
		return rune(k.b)
	case k.csi || k.mods&^modShift != 0:
		return 0
	case k.mods&modShift == 0:
		return rune(k.code)
	case k.shifted != 0:
		return rune(k.shifted)
	case k.code >= 'a' && k.code <= 'z':
		return rune(k.code - 'a' + 'A')
	}
	return 0
}

// nextKey decodes the first key of b; !ok means b may be a split CSI key.
// NOTE: a lone ESC is never held back: it is the Escape key.
func nextKey(b []byte) (k key, n int, ok bool) {
	if b[0] != escape {
		return key{plain: true, b: b[0]}, 1, true
	}
	if len(b) < 2 || b[1] != '[' {
		return key{csi: true}, 1, true
	}
	i := 2
	for i < len(b) && i < maxSequence && isParamByte(b[i]) {
		i++
	}
	switch {
	case i == len(b) && i > 2 && i < maxSequence:
		return key{}, 0, false
	case i == len(b) || i == maxSequence:
		return key{csi: true}, 1, true
	}
	params := strings.Split(string(b[2:i]), ";")
	switch b[i] {
	case 'u':
		if k, ok := csiUKey(params); ok {
			return k, i + 1, true
		}
	case '~':
		if k, ok := modifyOtherKey(params); ok {
			return k, i + 1, true
		}
	}
	return key{csi: true}, i + 1, true
}

func isParamByte(c byte) bool { return c >= '0' && c <= '9' || c == ';' || c == ':' }

// csiUKey decodes the kitty keyboard protocol's
// CSI code[:shifted[:base]] [; mods[:event] [; text]] u.
func csiUKey(params []string) (key, bool) {
	codes := strings.Split(params[0], ":")
	code, err := strconv.Atoi(codes[0])
	if err != nil {
		return key{}, false
	}
	k := key{code: code}
	if len(codes) > 1 && codes[1] != "" {
		k.shifted, _ = strconv.Atoi(codes[1])
	}
	if len(params) > 1 {
		mods := strings.Split(params[1], ":")
		if m, err := strconv.Atoi(mods[0]); err == nil && m > 0 {
			k.mods = (m - 1) &^ modLocks
		}
		k.release = len(mods) > 1 && mods[1] == "3"
	}
	return k, true
}

// modifyOtherKey decodes xterm's modifyOtherKeys CSI 27 ; mods ; code ~,
// whose code is the character typed, shifted already.
func modifyOtherKey(params []string) (key, bool) {
	if len(params) != 3 || params[0] != "27" {
		return key{}, false
	}
	m, err1 := strconv.Atoi(params[1])
	code, err2 := strconv.Atoi(params[2])
	if err1 != nil || err2 != nil || m < 1 {
		return key{}, false
	}
	k := key{code: code, mods: (m - 1) &^ modLocks}
	if k.mods&modShift != 0 {
		k.shifted = code
	}
	return k, true
}

// prefixFilter finds the prefix and its command key in an attached session's
// input. NOTE: a lone prefix waits, without timeout, for the next key.
type prefixFilter struct {
	prefix PrefixKey
	// switching binds the switch keys (n, p, s).
	switching bool
	// held is the start of an escape sequence split across reads.
	held []byte
	// armed is the prefix as typed, while it waits for the command key.
	armed []byte
}

func newPrefixFilter(k PrefixKey) *prefixFilter { return &prefixFilter{prefix: k} }

// isPrefix reports whether k is a press of the prefix key, in any encoding.
func (f *prefixFilter) isPrefix(k key) bool {
	switch {
	case k.plain:
		return k.b == f.prefix.controlByte()
	case k.csi || k.release:
		return false
	}
	return k.code == f.prefix.keyCode() && k.mods&^modShift == modCtrl
}

// filter consumes p up to its first command: it returns the input to forward,
// the command's action, and the input after it for the next call.
func (f *prefixFilter) filter(p []byte) (out []byte, act action, rest []byte) {
	buf := p
	if len(f.held) > 0 {
		buf = append(f.held, p...)
		f.held = nil
	}
	for len(buf) > 0 {
		k, n, ok := nextKey(buf)
		if !ok {
			f.held = append([]byte(nil), buf...)
			return out, actionNone, nil
		}
		raw := buf[:n]
		buf = buf[n:]
		if f.armed == nil {
			if f.isPrefix(k) {
				f.armed = append([]byte{}, raw...)
				continue
			}
			out = append(out, raw...)
			continue
		}
		if k.release {
			continue
		}
		prefix := f.armed
		f.armed = nil
		if f.isPrefix(k) {
			out = append(out, raw...)
			continue
		}
		if act := prefixBindings[k.char()]; act != actionNone && (f.switching || act.switchDir() == 0) {
			return out, act, buf
		}
		out = append(append(out, prefix...), raw...)
	}
	return out, actionNone, nil
}

// forwardInput copies term's input to is. When the user detaches or
// switches, it reports the action on ended, resets the terminal, closes the
// input and cancels the attach client as a ctx cancel does.
func forwardInput(term io.Reader, is InteractiveSession, o bridgeOptions, out *bridgeOutput, ended chan<- action) {
	if o.prefix.Enabled() {
		f := newPrefixFilter(o.prefix)
		f.switching = o.others != nil
		if act := pumpInput(is, term, f, out, o.others); act != actionNone {
			// Mute first: output arriving after the detach must not undo the reset.
			out.mute(terminalReset)
			ended <- act
			_ = sbx.CloseInput(is)
			_ = sbx.Cancel(is)
			return
		}
	} else {
		_, _ = io.Copy(is, term)
	}
	// The client closed its input; let the remote side see EOF.
	_ = sbx.CloseInput(is)
}

// pumpInput copies src to dst through f until src ends, or the user detaches
// or switches, which it returns. Help goes to out; so does a switch by name
// when others says there is no other session. Input after the action is
// dropped.
func pumpInput(dst io.Writer, src io.Reader, f *prefixFilter, out *bridgeOutput, others func() bool) action {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		data := buf[:n]
		for len(data) > 0 {
			fwd, act, rest := f.filter(data)
			if len(fwd) > 0 {
				if _, werr := dst.Write(fwd); werr != nil {
					return actionNone
				}
			}
			switch act {
			case actionHelp:
				_, _ = io.WriteString(out, prefixHelp(f.prefix, f.switching))
			case actionNext, actionPrev:
				if !others() {
					_, _ = io.WriteString(out, noOtherSession)
					break
				}
				return act
			case actionDetach, actionPick:
				return act
			case actionNone:
			}
			data = rest
		}
		if err != nil {
			if p := f.pending(); len(p) > 0 {
				_, _ = dst.Write(p)
			}
			return actionNone
		}
	}
}

// bridgeOutput keeps the program's output and Sundae's lines from interleaving.
type bridgeOutput struct {
	w     io.Writer
	mu    sync.Mutex
	muted bool
}

func (o *bridgeOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.muted {
		return len(p), nil
	}
	return o.w.Write(p) //nolint:wrapcheck
}

// mute writes last, then drops everything written after.
func (o *bridgeOutput) mute(last string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.muted {
		_, _ = io.WriteString(o.w, last)
	}
	o.muted = true
}

// pending returns and forgets the input held back, to pass on at end of input.
func (f *prefixFilter) pending() []byte {
	out := append(f.armed, f.held...)
	f.armed, f.held = nil, nil
	return out
}
