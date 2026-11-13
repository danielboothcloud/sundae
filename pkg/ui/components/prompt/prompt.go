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
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
	"github.com/danielboothcloud/sundae/pkg/ui/styles"
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
	// Multi picks any number of a list of options, shown as a vertical
	// checkbox list while focused: ↑↓ move the cursor (leaving the field at
	// the list's ends), space toggles the option under it, and typing
	// filters the list. Picked options stay at the top, even when the
	// filter hides them. Values are in the order they were picked.
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
	// cursor is a Multi field's row under the cursor; offset is the
	// first row shown.
	cursor, offset int

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
	return f
}

// multiRows is how many option rows a focused Multi field shows at most.
const multiRows = 7

// row is one line of a Multi field's list: an option, and whether the
// filter hides it (only picked options are shown when hidden).
type row struct {
	opt    int
	hidden bool
}

// rows returns a Multi field's list: the picked options in the order
// picked, then the unpicked ones matching the filter.
func (f *Field) rows() []row {
	var rr []row
	for _, v := range f.picked {
		for i, o := range f.options {
			if o.Value == v {
				rr = append(rr, row{opt: i, hidden: !f.matches(o)})
			}
		}
	}
	for i, o := range f.options {
		if !f.isPicked(o.Value) && f.matches(o) {
			rr = append(rr, row{opt: i})
		}
	}
	return rr
}

// clampCursor keeps a Multi field's cursor on a row and in view.
func (f *Field) clampCursor() {
	n := len(f.rows())
	f.cursor = max(min(f.cursor, n-1), 0)
	if f.cursor < f.offset {
		f.offset = f.cursor
	}
	if f.cursor >= f.offset+multiRows {
		f.offset = f.cursor - multiRows + 1
	}
	f.offset = max(min(f.offset, n-multiRows), 0)
}

// cursorTo puts a Multi field's cursor on the row of option i.
func (f *Field) cursorTo(i int) {
	for r, rw := range f.rows() {
		if rw.opt == i {
			f.cursor = r
		}
	}
	f.clampCursor()
}

// setMultiFilter sets a Multi field's filter, putting the cursor on the
// first unpicked match (else the first row).
func (f *Field) setMultiFilter(s string) {
	f.filter = s
	f.cursor = 0
	for r, rw := range f.rows() {
		if !f.isPicked(f.options[rw.opt].Value) {
			f.cursor = r
			break
		}
	}
	f.clampCursor()
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
	case Choice:
		for i, o := range f.options {
			if o.Value == v {
				f.choice = i
			}
		}
	case Multi:
		for i, o := range f.options {
			if o.Value == v {
				f.cursorTo(i)
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
	f.cursor, f.offset = 0, 0
	for i, o := range opts {
		if o.Value == selected {
			f.choice = i
		}
	}
}

// Selected returns a Choice field's chosen option; false when there is
// none, or none matches the filter.
func (f *Field) Selected() (Option, bool) {
	if f.kind == Multi {
		rr := f.rows()
		if f.cursor < 0 || f.cursor >= len(rr) {
			return Option{}, false
		}
		return f.options[rr[f.cursor].opt], true
	}
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
	for _, fd := range fields {
		if fd.kind == Text {
			// Typed text reads as the other fields' values do.
			st := fd.input.Styles()
			st.Focused.Text = c.Styles.Prompt.ActiveValue
			st.Blurred.Text = c.Styles.Prompt.Value
			fd.input.SetStyles(st)
		}
	}
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
		move := key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "move"))
		toggle := key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "pick"))
		filter := key.NewBinding(key.WithKeys("backspace"), key.WithHelp("type", "filter"))
		cancel := f.KeyMap.Cancel
		if cur.filter != "" {
			cancel = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter"))
		}
		return []key.Binding{move, toggle, filter, f.KeyMap.Submit, cancel}
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
	if cur != nil && cur.kind == Multi {
		switch km.String() {
		case "up", "ctrl+p":
			if cur.cursor > 0 {
				cur.cursor--
				cur.clampCursor()
			} else {
				f.move(-1)
			}
			return Pending, nil
		case "down", "ctrl+n":
			if cur.cursor < len(cur.rows())-1 {
				cur.cursor++
				cur.clampCursor()
			} else {
				f.move(1)
			}
			return Pending, nil
		}
	}
	switch km.String() {
	case "esc":
		if cur != nil && cur.kind == Multi && cur.filter != "" {
			cur.setMultiFilter("")
			return Pending, nil
		}
		if cur != nil && cur.kind == Choice && cur.filter != "" {
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
		case "space", " ":
			if rr := cur.rows(); cur.cursor < len(rr) {
				i := rr[cur.cursor].opt
				v := cur.options[i].Value
				cur.toggle(v, !cur.isPicked(v))
				// The cursor follows the option to its new row; an
				// unpicked option the filter hides leaves the list.
				cur.cursorTo(i)
				cur.clampCursor()
				changed = true
			}
		case "backspace":
			if cur.filter != "" {
				r := []rune(cur.filter)
				cur.setMultiFilter(string(r[:len(r)-1]))
			}
		default:
			if km.Text != "" && km.Text != " " {
				cur.setMultiFilter(cur.filter + km.Text)
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

// View renders the form in width columns, with the shared prompt styles.
func (f *Form) View(width int) string {
	ps := f.common.Styles.Prompt
	labelW := 0
	for _, fd := range f.fields {
		labelW = max(labelW, lipgloss.Width(fd.Label))
	}
	labelW += 3
	lines := []string{ps.Title.Render(f.Title), ""}
	for i, fd := range f.fields {
		focused := i == f.focus
		labelStyle, valueStyle := ps.Label, ps.Value
		if focused {
			labelStyle, valueStyle = ps.ActiveLabel, ps.ActiveValue
		}
		label := labelStyle.Width(labelW).Render(fd.Label)
		var value string
		switch fd.kind {
		case Fixed:
			value = ps.Value.Render(fd.fixed)
		case Text:
			value = fd.input.View()
		case Choice:
			o, ok := fd.Selected()
			name := "(no match)"
			if ok {
				name = o.label()
			}
			value = valueStyle.Render("‹ " + name + " ›")
			var extra []string
			if fd.filter != "" {
				extra = append(extra, "/"+fd.filter)
			}
			if ok && o.Detail != "" {
				extra = append(extra, o.Detail)
			}
			if len(extra) > 0 {
				value += "   " + ps.Detail.Render(strings.Join(extra, "  "))
			}
		case Multi:
			if focused {
				lines = append(lines, fd.multiView(f.common.Styles, label, labelW, width)...)
				continue
			}
			value = valueStyle.Render(fd.summary())
		case Toggle:
			v := "no"
			if fd.on {
				v = "yes"
			}
			value = valueStyle.Render(v)
		}
		lines = append(lines, common.TruncateString("  "+label+value, max(width, 1)))
	}
	if f.err != nil {
		lines = append(lines, "", ps.Error.Render(common.TruncateString("✗ "+f.err.Error(), max(width, 1))))
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

// View renders the question, in the prompt's question style, with the
// answers at the right end of width columns as the help footer renders
// keys ("y yes • N no"), cutting the question to fit.
func (c *Confirm) View(width int) string {
	answer := footer.NewHelp(c.common).ShortHelpView([]key.Binding{c.Yes, c.No})
	aw := lipgloss.Width(answer)
	q := common.TruncateString(c.Question, max(width-aw-2, 1))
	gap := max(width-lipgloss.Width(q)-aw, 2)
	return c.common.Styles.Prompt.Question.Render(q) + strings.Repeat(" ", gap) + answer
}

// summary is a Multi field's picked options, shortened: "a, b +2".
func (f *Field) summary() string {
	n := len(f.picked)
	switch {
	case n == 0:
		return "none picked"
	case n <= 2:
		return strings.Join(f.picked, ", ")
	}
	return f.picked[0] + ", " + f.picked[1] + " +" + itoa(n-2)
}

// multiView renders a focused Multi field: the label with the filter and
// the picked count, then the list's visible rows, with more-indicators.
func (f *Field) multiView(st *styles.Styles, label string, labelW, width int) []string {
	ps := st.Prompt
	w := max(width, 1)
	filter := ps.Detail.Render("type to filter")
	if f.filter != "" {
		filter = ps.ActiveValue.Render(f.filter)
	}
	head := "  " + label + ps.Detail.Render("filter: ") + filter + ps.ActiveValue.Render("█")
	count := ps.Detail.Render(itoa(len(f.picked)) + " of " + itoa(len(f.options)) + " picked")
	if gap := w - 1 - lipgloss.Width(head) - lipgloss.Width(count); gap >= 2 {
		head += strings.Repeat(" ", gap) + count
	} else {
		head += "  " + count
	}
	lines := []string{common.TruncateString(head, w)}
	indent := strings.Repeat(" ", 2+labelW)
	rr := f.rows()
	f.clampCursor()
	more := func(s string) string {
		return common.TruncateString(indent+ps.Detail.Render(s), w)
	}
	if f.offset > 0 {
		lines = append(lines, more("  ↑ "+itoa(f.offset)+" more"))
	}
	if len(rr) == 0 {
		lines = append(lines, more("  (no match)"))
	}
	end := min(f.offset+multiRows, len(rr))
	for r := f.offset; r < end; r++ {
		rw := rr[r]
		o := f.options[rw.opt]
		mark := "[ ] "
		if f.isPicked(o.Value) {
			mark = "[x] "
		}
		pointer, style := "  ", ps.Value
		if r == f.cursor {
			pointer, style = "▸ ", ps.ActiveValue
		}
		line := indent + style.Render(pointer+mark+o.label())
		if rw.hidden {
			line += "  " + ps.Detail.Render("(hidden by filter)")
		} else if o.Detail != "" {
			line += "  " + ps.Detail.Render(o.Detail)
		}
		lines = append(lines, common.TruncateString(line, w))
	}
	if rest := len(rr) - end; rest > 0 {
		lines = append(lines, more("  ↓ "+itoa(rest)+" more"))
	}
	return lines
}
