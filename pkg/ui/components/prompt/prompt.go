// Package prompt holds the TUI's small input components: a Form of labelled
// fields (fixed text, free text, a choice among options, a multi-select,
// a yes/no toggle),
// and a one-line yes/no Confirm. They are embedded by pages, which forward
// messages to them and act on the Result.
package prompt

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// Result is what a message did to a prompt.
type Result int

const (
	// Pending: the prompt is still open.
	Pending Result = iota
	// Submitted: the user confirmed, and the form's values are valid.
	Submitted
	// Cancelled: the user dismissed the prompt.
	Cancelled
)

// Kind is the kind of a Field.
type Kind int

// Field kinds.
const (
	// Fixed shows a value the user cannot change. It never takes focus.
	Fixed Kind = iota
	// Text is free text.
	Text
	// Choice picks one of a list of options with ←→; a filterable choice
	// narrows the options to those containing what the user types.
	Choice
	// Toggle is yes or no.
	Toggle
	// Multi picks any number of a list of options: ←→ move among them,
	// space toggles the one shown, and typing narrows them to those
	// containing what was typed. Values are in the order they were picked.
	Multi
)

// Option is one choice of a Choice field.
type Option struct {
	// Value identifies the option.
	Value string
	// Label is shown for the option; Value when empty.
	Label string
	// Detail is shown after the chosen option, dimmed.
	Detail string
}

func (o Option) label() string {
	if o.Label != "" {
		return o.Label
	}
	return o.Value
}

// Field is one labelled input of a Form.
type Field struct {
	// Key identifies the field in its Form.
	Key string
	// Label is shown before the value.
	Label string

	kind  Kind
	fixed string
	input textinput.Model
	// Validate checks a Text field's value; its error keeps the form open.
	validate func(string) error
	edited   bool

	options    []Option
	choice     int
	filterable bool
	filter     string

	// picked are a Multi field's selected Values, in the order picked.
	picked []string

	on bool
}

// FixedField returns a field showing value.
func FixedField(key, label, value string) *Field {
	return &Field{Key: key, Label: label, kind: Fixed, fixed: value}
}

// TextField returns a free-text field holding value. validate, when not
// nil, checks the value when the form is submitted.
func TextField(key, label, value string, validate func(string) error) *Field {
	in := textinput.New()
	in.Prompt = ""
	in.SetValue(value)
	in.CharLimit = 64
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	return &Field{Key: key, Label: label, kind: Text, input: in, validate: validate}
}

// ChoiceField returns a field choosing among opts, with the option whose
// Value is selected chosen (else the first). A filterable choice narrows
// the options to those containing what the user types.
func ChoiceField(key, label string, opts []Option, selected string, filterable bool) *Field {
	f := &Field{Key: key, Label: label, kind: Choice, filterable: filterable}
	f.SetOptions(opts, selected)
	return f
}

// MultiField returns a field picking any number of opts, with the Values in
// picked selected, in that order. Typing filters the options.
func MultiField(key, label string, opts []Option, picked []string) *Field {
	f := &Field{Key: key, Label: label, kind: Multi, filterable: true}
	f.SetOptions(opts, "")
	for _, v := range picked {
		f.toggle(v, true)
	}
	if len(f.picked) > 0 {
		f.SetValue(f.picked[0])
	}
	return f
}

// Values returns a Multi field's selected Values, in the order picked.
func (f *Field) Values() []string { return append([]string(nil), f.picked...) }

// toggle selects (on) or deselects the option with Value v.
func (f *Field) toggle(v string, on bool) {
	i := -1
	for j, p := range f.picked {
		if p == v {
			i = j
		}
	}
	switch {
	case on && i < 0:
		for _, o := range f.options {
			if o.Value == v {
				f.picked = append(f.picked, v)
			}
		}
	case !on && i >= 0:
		f.picked = append(f.picked[:i], f.picked[i+1:]...)
	}
}

// isPicked reports whether a Multi field has v selected.
func (f *Field) isPicked(v string) bool {
	for _, p := range f.picked {
		if p == v {
			return true
		}
	}
	return false
}

// ToggleField returns a yes/no field.
func ToggleField(key, label string, on bool) *Field {
	return &Field{Key: key, Label: label, kind: Toggle, on: on}
}

// SetCharLimit sets how many characters a Text field takes; 64 by default.
func (f *Field) SetCharLimit(n int) {
	if f.kind == Text {
		f.input.CharLimit = n
	}
}

// Kind returns the field's kind.
func (f *Field) Kind() Kind { return f.kind }

// Value returns the field's value: the text, the chosen option's Value
// ("" when no option matches the filter), or "yes" / "no".
func (f *Field) Value() string {
	switch f.kind {
	case Fixed:
		return f.fixed
	case Text:
		return strings.TrimSpace(f.input.Value())
	case Choice:
		if o, ok := f.Selected(); ok {
			return o.Value
		}
		return ""
	case Multi:
		return strings.Join(f.picked, ",")
	case Toggle:
		if f.on {
			return "yes"
		}
		return "no"
	}
	return ""
}

// On reports whether a Toggle field is on.
func (f *Field) On() bool { return f.on }

// Edited reports whether the user changed a Text field.
func (f *Field) Edited() bool { return f.edited }

// SetValue sets a Fixed or Text field's value, selects a Choice field's
// option by Value, or sets a Toggle field ("yes" is on). It does not mark
// the field edited.
func (f *Field) SetValue(v string) {
	switch f.kind {
	case Fixed:
		f.fixed = v
	case Text:
		f.input.SetValue(v)
		f.input.CursorEnd()
	case Choice, Multi:
		for i, o := range f.options {
			if o.Value == v {
				f.choice = i
			}
		}
	case Toggle:
		f.on = v == "yes"
	}
}

// SetOptions replaces a Choice field's options and selects the one whose
// Value is selected (else the first).
func (f *Field) SetOptions(opts []Option, selected string) {
	f.options = opts
	f.choice = 0
	f.filter = ""
	for i, o := range opts {
		if o.Value == selected {
			f.choice = i
		}
	}
}

// Selected returns a Choice field's chosen option; false when there is
// none, or none matches the filter.
func (f *Field) Selected() (Option, bool) {
	if (f.kind != Choice && f.kind != Multi) || len(f.options) == 0 || !f.matches(f.options[f.choice]) {
		return Option{}, false
	}
	return f.options[f.choice], true
}

// matches reports whether o matches the filter.
func (f *Field) matches(o Option) bool {
	return f.filter == "" || strings.Contains(strings.ToLower(o.label()), strings.ToLower(f.filter))
}

// visible returns the indices of the options matching the filter.
func (f *Field) visible() []int {
	var idx []int
	for i, o := range f.options {
		if f.matches(o) {
			idx = append(idx, i)
		}
	}
	return idx
}

// step moves a Choice field's choice by d among the matching options.
func (f *Field) step(d int) bool {
	vis := f.visible()
	if len(vis) == 0 {
		return false
	}
	at := 0
	for i, idx := range vis {
		if idx == f.choice {
			at = i
		}
	}
	if !f.matches(f.options[f.choice]) {
		f.choice = vis[0]
		return true
	}
	next := vis[(at+d+len(vis))%len(vis)]
	changed := next != f.choice
	f.choice = next
	return changed
}

// setFilter sets a filterable Choice field's filter and keeps the choice on
// a matching option.
func (f *Field) setFilter(s string) {
	f.filter = s
	if len(f.options) > 0 && !f.matches(f.options[f.choice]) {
		if vis := f.visible(); len(vis) > 0 {
			f.choice = vis[0]
		}
	}
}

// focusable reports whether the field can take focus.
func (f *Field) focusable() bool { return f.kind != Fixed }

// KeyMap is the keys of a Form.
type KeyMap struct {
	Field  key.Binding
	Change key.Binding
	Submit key.Binding
	Cancel key.Binding
}

// Form is a titled list of fields. Up and down move between fields, left
// and right change a choice or toggle, enter submits and esc cancels.
type Form struct {
	common common.Common
	// Title is shown above the fields.
	Title string
	// Validate checks the form as a whole when it is submitted, after
	// each Text field's own check. Its error is shown and keeps the form
	// open.
	Validate func(*Form) error
	// OnChange is called after the user changed a field's value.
	OnChange func(f *Form, key string)
	// KeyMap is the form's keys, for help.
	KeyMap KeyMap

	fields []*Field
	focus  int
	err    error
}

// NewForm returns a form of fields, focused on the first that can take
// focus. submit names what enter does in the help ("create").
func NewForm(c common.Common, title, submit string, fields ...*Field) *Form {
	f := &Form{common: c, Title: title, fields: fields, focus: -1}
	f.KeyMap = KeyMap{
		Field:  key.NewBinding(key.WithKeys("up", "down", "tab", "shift+tab"), key.WithHelp("↑↓", "field")),
		Change: key.NewBinding(key.WithKeys("left", "right"), key.WithHelp("←→", "change")),
		Submit: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", submit)),
		Cancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
	f.move(1)
	return f
}

// Field returns the field with the key; nil when there is none.
func (f *Form) Field(key string) *Field {
	for _, fd := range f.fields {
		if fd.Key == key {
			return fd
		}
	}
	return nil
}

// Focused returns the focused field; nil when none can take focus.
func (f *Form) Focused() *Field {
	if f.focus < 0 || f.focus >= len(f.fields) {
		return nil
	}
	return f.fields[f.focus]
}

// Err returns the error shown, if any.
func (f *Form) Err() error { return f.err }

// SetError shows err under the fields; nil clears it.
func (f *Form) SetError(err error) { f.err = err }

// move focuses the next field that can take focus in direction d.
func (f *Form) move(d int) {
	n := len(f.fields)
	if n == 0 {
		return
	}
	if cur := f.Focused(); cur != nil && cur.kind == Text {
		cur.input.Blur()
	}
	i := f.focus
	for range n {
		i = (i + d + n) % n
		if f.fields[i].focusable() {
			f.focus = i
			break
		}
	}
	if cur := f.Focused(); cur != nil && cur.kind == Text {
		cur.input.Focus()
		cur.input.CursorEnd()
	}
}

// ShortHelp returns the form's keys for the help line.
func (f *Form) ShortHelp() []key.Binding {
	if cur := f.Focused(); cur != nil && cur.kind == Multi {
		toggle := key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "pick"))
		return []key.Binding{f.KeyMap.Field, f.KeyMap.Change, toggle, f.KeyMap.Submit, f.KeyMap.Cancel}
	}
	return []key.Binding{f.KeyMap.Field, f.KeyMap.Change, f.KeyMap.Submit, f.KeyMap.Cancel}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Update handles a message and reports whether the form was submitted or
// cancelled. A submitted form's values are valid.
func (f *Form) Update(msg tea.Msg) (Result, tea.Cmd) {
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		if cur := f.Focused(); cur != nil && cur.kind == Text {
			var cmd tea.Cmd
			cur.input, cmd = cur.input.Update(msg)
			return Pending, cmd
		}
		return Pending, nil
	}
	cur := f.Focused()
	switch km.String() {
	case "esc":
		if cur != nil && (cur.kind == Choice || cur.kind == Multi) && cur.filter != "" {
			cur.setFilter("")
			return Pending, nil
		}
		return Cancelled, nil
	case "enter":
		if err := f.check(); err != nil {
			f.err = err
			return Pending, nil
		}
		f.err = nil
		return Submitted, nil
	case "up", "shift+tab":
		f.move(-1)
		return Pending, nil
	case "down", "tab":
		f.move(1)
		return Pending, nil
	}
	if cur == nil {
		return Pending, nil
	}
	changed := false
	var cmd tea.Cmd
	switch cur.kind {
	case Choice:
		switch km.String() {
		case "left":
			changed = cur.step(-1)
		case "right":
			changed = cur.step(1)
		case "backspace":
			if cur.filterable && cur.filter != "" {
				r := []rune(cur.filter)
				cur.setFilter(string(r[:len(r)-1]))
				changed = true
			}
		default:
			if cur.filterable && km.Text != "" {
				cur.setFilter(cur.filter + km.Text)
				changed = true
			}
		}
	case Multi:
		switch km.String() {
		case "left":
			cur.step(-1)
		case "right":
			cur.step(1)
		case "space", " ":
			if o, ok := cur.Selected(); ok {
				cur.toggle(o.Value, !cur.isPicked(o.Value))
				changed = true
			}
		case "backspace":
			if cur.filter != "" {
				r := []rune(cur.filter)
				cur.setFilter(string(r[:len(r)-1]))
			}
		default:
			if km.Text != "" && km.Text != " " {
				cur.setFilter(cur.filter + km.Text)
			}
		}
	case Toggle:
		switch km.String() {
		case "left", "right", "space", " ":
			cur.on = !cur.on
			changed = true
		case "y":
			changed, cur.on = !cur.on, true
		case "n":
			changed, cur.on = cur.on, false
		}
	case Text:
		before := cur.input.Value()
		cur.input, cmd = cur.input.Update(msg)
		if cur.input.Value() != before {
			cur.edited = true
			changed = true
		}
	}
	if changed {
		f.err = nil
		if f.OnChange != nil {
			f.OnChange(f, cur.Key)
		}
	}
	return Pending, cmd
}

// check validates every field, then the form.
func (f *Form) check() error {
	for _, fd := range f.fields {
		switch fd.kind {
		case Text:
			if fd.validate != nil {
				if err := fd.validate(fd.Value()); err != nil {
					return err
				}
			}
		case Choice:
			if _, ok := fd.Selected(); !ok {
				return &choiceError{label: fd.Label}
			}
		case Multi:
			if len(fd.picked) == 0 {
				return &multiError{label: fd.Label}
			}
		}
	}
	if f.Validate != nil {
		return f.Validate(f)
	}
	return nil
}

type choiceError struct{ label string }

func (e *choiceError) Error() string { return "choose a " + strings.ToLower(e.label) }

type multiError struct{ label string }

func (e *multiError) Error() string {
	return "pick at least one of " + strings.ToLower(e.label) + " (space toggles)"
}

// View renders the form in width columns.
func (f *Form) View(width int) string {
	st := f.common.Styles
	labelW := 0
	for _, fd := range f.fields {
		labelW = max(labelW, lipgloss.Width(fd.Label))
	}
	labelW += 3
	title := st.RepoSelector.Normal.Title.Render(f.Title)
	lines := []string{title, ""}
	dim := st.RepoSelector.Normal.Desc
	for i, fd := range f.fields {
		focused := i == f.focus
		label := lipgloss.NewStyle().Width(labelW).Render(fd.Label)
		if focused {
			label = st.RepoSelector.Active.Title.Width(labelW).Render(fd.Label)
		}
		var value string
		switch fd.kind {
		case Fixed:
			value = fd.fixed
		case Text:
			value = fd.input.View()
		case Choice:
			o, ok := fd.Selected()
			name := "(no match)"
			if ok {
				name = o.label()
			}
			value = "‹ " + name + " ›"
			if focused {
				value = st.RepoSelector.Active.Title.Render(value)
			}
			var extra []string
			if fd.filter != "" {
				extra = append(extra, "/"+fd.filter)
			}
			if ok && o.Detail != "" {
				extra = append(extra, o.Detail)
			}
			if len(extra) > 0 {
				value += "   " + dim.Render(strings.Join(extra, "  "))
			}
		case Multi:
			o, ok := fd.Selected()
			name := "(no match)"
			if ok {
				mark := "[ ] "
				if fd.isPicked(o.Value) {
					mark = "[x] "
				}
				name = mark + o.label()
			}
			value = "‹ " + name + " ›"
			if focused {
				value = st.RepoSelector.Active.Title.Render(value)
			}
			var extra []string
			if fd.filter != "" {
				extra = append(extra, "/"+fd.filter)
			}
			switch n := len(fd.picked); {
			case n == 0:
				extra = append(extra, "none picked")
			case n <= 2:
				extra = append(extra, strings.Join(fd.picked, ", "))
			default:
				extra = append(extra, fd.picked[0]+", "+fd.picked[1]+", +"+itoa(n-2))
			}
			value += "   " + dim.Render(strings.Join(extra, "  "))
		case Toggle:
			value = "no"
			if fd.on {
				value = "yes"
			}
			if focused {
				value = st.RepoSelector.Active.Title.Render(value)
			}
		}
		line := "  " + label + value
		lines = append(lines, common.TruncateString(line, max(width, 1)))
	}
	if f.err != nil {
		lines = append(lines, "", st.State.Error.Render(common.TruncateString("✗ "+f.err.Error(), max(width, 1))))
	}
	return strings.Join(lines, "\n")
}

// Confirm is a one-line yes/no question; no is the default. y confirms,
// any other key cancels.
type Confirm struct {
	common common.Common
	// Question is what is asked.
	Question string
	// KeyMap is the confirm's keys, for help.
	Yes, No key.Binding
}

// NewConfirm returns a confirm asking question.
func NewConfirm(c common.Common, question string) *Confirm {
	return &Confirm{
		common:   c,
		Question: question,
		Yes:      key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y", "yes")),
		No:       key.NewBinding(key.WithKeys("n", "N", "esc", "enter"), key.WithHelp("N", "no")),
	}
}

// Update handles a message: y submits, any other key cancels.
func (c *Confirm) Update(msg tea.Msg) Result {
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return Pending
	}
	if key.Matches(km, c.Yes) {
		return Submitted
	}
	return Cancelled
}

// View renders the question with "y / N" at the right end of width
// columns, cutting the question to fit.
func (c *Confirm) View(width int) string {
	st := c.common.Styles
	answer := st.HelpKey.Render("y") + st.HelpValue.Render(" / ") + st.HelpKey.Render("N")
	aw := lipgloss.Width(answer)
	q := common.TruncateString(c.Question, max(width-aw-2, 1))
	gap := max(width-lipgloss.Width(q)-aw, 2)
	return q + strings.Repeat(" ", gap) + answer
}
