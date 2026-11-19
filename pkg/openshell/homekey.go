package openshell

import (
	"errors"
	"io"
	"strconv"
	"strings"

	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
)

// ErrHome is returned by Bridge, with code 0, when the user pressed the home
// key to go to the Sessions screen. The session's program keeps running.
var ErrHome = errors.New("left for the Sessions screen")

// HomeKey is the character typed with Ctrl to leave an attached session for
// the Sessions screen (']' for Ctrl-]); the zero HomeKey is none.
type HomeKey byte

// DefaultHomeKey is Ctrl-]: telnet's escape, unused by shells and editors.
const DefaultHomeKey HomeKey = ']'

// Enabled reports whether there is a home key.
func (k HomeKey) Enabled() bool { return k != 0 }

// String names the key for people, e.g. "Ctrl-]" or "Ctrl-A"; "" for none.
func (k HomeKey) String() string {
	if !k.Enabled() {
		return ""
	}
	return "Ctrl-" + strings.ToUpper(string(rune(k)))
}

// controlByte is the byte a terminal sends for the key in its plain encoding.
func (k HomeKey) controlByte() byte { return byte(k) & 0x1f }

// keyCode is the key's code in the CSI-u and modifyOtherKeys encodings: the
// unshifted character.
func (k HomeKey) keyCode() int { return int(strings.ToLower(string(rune(k)))[0]) }

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
	// code and mods are a CSI-u or modifyOtherKeys key: its code and the
	// modifier bits.
	code, mods int
	// release is a key release (kitty's report event types).
	release bool
	// csi is any other escape sequence or byte, passed through untouched.
	csi bool
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
	if len(params) > 1 {
		mods := strings.Split(params[1], ":")
		if m, err := strconv.Atoi(mods[0]); err == nil && m > 0 {
			k.mods = (m - 1) &^ modLocks
		}
		k.release = len(mods) > 1 && mods[1] == "3"
	}
	return k, true
}

// modifyOtherKey decodes xterm's modifyOtherKeys CSI 27 ; mods ; code ~.
func modifyOtherKey(params []string) (key, bool) {
	if len(params) != 3 || params[0] != "27" {
		return key{}, false
	}
	m, err1 := strconv.Atoi(params[1])
	code, err2 := strconv.Atoi(params[2])
	if err1 != nil || err2 != nil || m < 1 {
		return key{}, false
	}
	return key{code: code, mods: (m - 1) &^ modLocks}, true
}

// homeFilter finds the home key in an attached session's input.
type homeFilter struct {
	home HomeKey
	// held is the start of an escape sequence split across reads.
	held []byte
}

// isHome reports whether k is a press of the home key, in any encoding.
func (f *homeFilter) isHome(k key) bool {
	switch {
	case k.plain:
		return k.b == f.home.controlByte()
	case k.csi || k.release:
		return false
	}
	return k.code == f.home.keyCode() && k.mods&^modShift == modCtrl
}

// filter returns the input of p to forward, up to the home key, and whether
// the home key was pressed; input after it is not returned.
func (f *homeFilter) filter(p []byte) (out []byte, home bool) {
	buf := p
	if len(f.held) > 0 {
		buf = append(f.held, p...)
		f.held = nil
	}
	for len(buf) > 0 {
		k, n, ok := nextKey(buf)
		if !ok {
			f.held = append([]byte(nil), buf...)
			return out, false
		}
		if f.isHome(k) {
			return out, true
		}
		out = append(out, buf[:n]...)
		buf = buf[n:]
	}
	return out, false
}

// pending returns and forgets the input held back, to pass on at end of input.
func (f *homeFilter) pending() []byte {
	p := f.held
	f.held = nil
	return p
}

// forwardInput copies term's input to is. On the home key it reports it on
// home, resets the terminal, closes the input and cancels the attach client
// as a ctx cancel does.
func forwardInput(term io.Reader, is InteractiveSession, k HomeKey, out *statusBar, home chan<- struct{}) {
	if !k.Enabled() {
		_, _ = io.Copy(is, term)
	} else if pumpInput(is, term, &homeFilter{home: k}) {
		// Leave first: output arriving after the home key must not undo the reset.
		out.leave()
		home <- struct{}{}
		_ = sbx.CloseInput(is)
		_ = sbx.Cancel(is)
		return
	}
	// The client closed its input; let the remote side see EOF.
	_ = sbx.CloseInput(is)
}

// pumpInput copies src to dst through f until src ends or the home key is
// pressed, which it reports. Input after the home key is dropped.
func pumpInput(dst io.Writer, src io.Reader, f *homeFilter) bool {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		fwd, home := f.filter(buf[:n])
		if len(fwd) > 0 {
			if _, werr := dst.Write(fwd); werr != nil {
				return false
			}
		}
		if home {
			return true
		}
		if err != nil {
			if p := f.pending(); len(p) > 0 {
				_, _ = dst.Write(p)
			}
			return false
		}
	}
}
