package prompt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
)

// press returns the key press for a key name ("enter", "left") or text.
func press(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

func newCommon() common.Common { return common.NewCommon(context.Background(), 80, 24) }

func newTestForm() *Form {
	notEmpty := func(s string) error {
		if s == "" {
			return errors.New("name is required")
		}
		return nil
	}
	return NewForm(newCommon(), "New session", "create",
		FixedField("repo", "Repository", "sundae"),
		TextField("name", "Name", "claude-1", notEmpty),
		ChoiceField("profile", "Profile", []Option{
			{Value: "claude", Detail: "img-claude · claude"},
			{Value: "codex", Detail: "img-codex · codex"},
		}, "codex", false),
		ToggleField("attach", "Attach", true),
	)
}

func TestFormFocusSkipsFixed(t *testing.T) {
	f := newTestForm()
	if got := f.Focused().Key; got != "name" {
		t.Fatalf("focused %q, want name", got)
	}
	f.Update(press("down"))
	f.Update(press("down"))
	if got := f.Focused().Key; got != "attach" {
		t.Fatalf("focused %q, want attach", got)
	}
	f.Update(press("down")) // wraps past the fixed field
	if got := f.Focused().Key; got != "name" {
		t.Fatalf("focused %q after wrap, want name", got)
	}
	f.Update(press("up"))
	if got := f.Focused().Key; got != "attach" {
		t.Fatalf("focused %q after up, want attach", got)
	}
}

func TestFormChangesAndOnChange(t *testing.T) {
	f := newTestForm()
	var changed []string
	f.OnChange = func(_ *Form, key string) { changed = append(changed, key) }
	if got := f.Field("profile").Value(); got != "codex" {
		t.Fatalf("initial profile %q", got)
	}
	f.Update(press("down"))
	f.Update(press("right"))
	if got := f.Field("profile").Value(); got != "claude" {
		t.Fatalf("profile %q after right, want claude (wraps)", got)
	}
	f.Update(press("down"))
	f.Update(press("left"))
	if f.Field("attach").On() {
		t.Fatal("attach still on after left")
	}
	f.Update(press("y"))
	if !f.Field("attach").On() {
		t.Fatal("attach off after y")
	}
	if strings.Join(changed, ",") != "profile,attach,attach" {
		t.Fatalf("OnChange calls %v", changed)
	}
}

func TestFormTextEditAndValidation(t *testing.T) {
	f := newTestForm()
	name := f.Field("name")
	if name.Edited() {
		t.Fatal("edited before typing")
	}
	// In normal mode keys do not reach the text.
	f.Update(press("backspace"))
	f.Update(press("x"))
	if name.Edited() || name.Value() != "claude-1" || f.Typing() {
		t.Fatalf("normal mode typed: %q", name.Value())
	}
	f.Update(press("/"))
	if !f.Typing() {
		t.Fatal("/ did not start typing")
	}
	for range len("claude-1") {
		f.Update(press("backspace"))
	}
	if !name.Edited() || name.Value() != "" {
		t.Fatalf("value %q edited %v", name.Value(), name.Edited())
	}
	// enter ends typing; a second enter submits.
	if res, _ := f.Update(press("enter")); res != Pending || f.Typing() {
		t.Fatalf("enter while typing: %v typing %v", res, f.Typing())
	}
	if res, _ := f.Update(press("enter")); res != Pending {
		t.Fatalf("submitted with an empty name: %v", res)
	}
	if f.Err() == nil || !strings.Contains(f.View(80), "name is required") {
		t.Fatalf("error not shown: %v\n%s", f.Err(), f.View(80))
	}
	f.Update(press("i"))
	for _, r := range "tui" {
		f.Update(press(string(r)))
	}
	if f.Err() != nil {
		t.Fatal("error not cleared by typing")
	}
	f.Validate = func(f *Form) error {
		if f.Field("name").Value() == "tui" {
			return errors.New("tui exists")
		}
		return nil
	}
	f.Update(press("enter")) // ends typing
	if res, _ := f.Update(press("enter")); res != Pending || f.Err() == nil {
		t.Fatalf("form validation ignored: %v %v", res, f.Err())
	}
	f.Update(press("/"))
	f.Update(press("2"))
	f.Update(press("esc")) // esc ends typing, it does not cancel
	if res, _ := f.Update(press("enter")); res != Submitted {
		t.Fatalf("result %v, err %v", res, f.Err())
	}
	if got := name.Value(); got != "tui2" {
		t.Fatalf("name %q", got)
	}
}

func TestFormFilterableChoice(t *testing.T) {
	opts := []Option{{Value: "dotfiles"}, {Value: "infra"}, {Value: "sundae"}, {Value: "scratch"}}
	f := NewForm(newCommon(), "New session", "create", ChoiceField("repo", "Repository", opts, "", true))
	repo := f.Field("repo")
	f.Update(press("l"))
	if got := repo.Value(); got != "infra" {
		t.Fatalf("after l: %q", got)
	}
	f.Update(press("h"))
	f.Update(press("/"))
	f.Update(press("s"))
	if got := repo.Value(); got != "dotfiles" {
		t.Fatalf("after s: %q", got)
	}
	f.Update(press("c"))
	if got := repo.Value(); got != "scratch" {
		t.Fatalf("after sc: %q", got)
	}
	f.Update(press("x"))
	if _, ok := repo.Selected(); ok {
		t.Fatal("a choice matches scx")
	}
	if res, _ := f.Update(press("enter")); res != Pending || f.Typing() {
		t.Fatal("enter while typing did not just end typing")
	}
	if res, _ := f.Update(press("enter")); res != Pending || f.Err() == nil {
		t.Fatal("submitted without a choice")
	}
	f.Update(press("/"))
	f.Update(press("backspace"))
	f.Update(press("backspace"))
	f.Update(press("esc")) // ends typing, keeps the filter
	f.Update(press("l"))
	if got := repo.Value(); got != "dotfiles" {
		t.Fatalf("after s, l: %q (wraps from scratch)", got)
	}
	if res, _ := f.Update(press("esc")); res != Pending {
		t.Fatal("esc with a filter cancelled the form")
	}
	if res, _ := f.Update(press("esc")); res != Cancelled {
		t.Fatal("esc without a filter did not cancel")
	}
}

func TestFormView(t *testing.T) {
	f := newTestForm()
	got := ansi.Strip(f.View(60))
	want := strings.Join([]string{
		"New session" + strings.Repeat(" ", 60-11-8) + " NORMAL ",
		"",
		"  Repository   sundae",
		"▸ Name         claude-1   / to edit",
		"  Profile      ‹ codex ›   img-codex · codex",
		"  Attach       yes",
	}, "\n")
	if got != want {
		t.Fatalf("view:\n%s\nwant:\n%s", got, want)
	}
	f.Update(press("/"))
	got = ansi.Strip(f.View(60))
	if !strings.Contains(got, "TYPING") || !strings.Contains(got, "▸ Name         claude-1 ") || strings.Contains(got, "/ to edit") {
		t.Fatalf("typing view:\n%s", got)
	}
	f.Update(press("esc"))
	f.Update(press("j"))
	got = ansi.Strip(f.View(60))
	if !strings.Contains(got, "▸ Profile") || !strings.Contains(got, "  Name         claude-1\n") {
		t.Fatalf("after j:\n%s", got)
	}
	// A form with nothing to type has no badge.
	plain := NewForm(newCommon(), "Import", "import", ToggleField("t", "T", false))
	if v := ansi.Strip(plain.View(60)); strings.Contains(v, "NORMAL") {
		t.Fatalf("badge without typing:\n%s", v)
	}
}

func TestConfirm(t *testing.T) {
	c := NewConfirm(newCommon(), "delete", "delete?", "loses sandbox, storage, unpushed work")
	for _, k := range []string{"y", "Y"} {
		if c.Update(press(k)) != Submitted {
			t.Fatalf("%s did not confirm", k)
		}
	}
	for _, k := range []string{"n", "N", "esc"} {
		if c.Update(press(k)) != Cancelled {
			t.Fatalf("%s did not cancel", k)
		}
	}
	// No stray key answers: enter, q, d and the like are ignored.
	for _, k := range []string{"enter", "q", "d", "x", "j", "space"} {
		if c.Update(press(k)) != Pending {
			t.Fatalf("%s answered the confirm", k)
		}
	}
	if c.Update(tea.WindowSizeMsg{}) != Pending {
		t.Fatal("a non-key message closed the confirm")
	}
	if got := ansi.Strip(c.Keys()); got != "y delete • esc keep" {
		t.Fatalf("keys %q", got)
	}
	if !c.Destructive() {
		t.Fatal("delete is not destructive")
	}
	push := NewConfirm(newCommon(), "push", "push main → github?", "fast-forward only, no force")
	if push.Destructive() || ansi.Strip(push.Keys()) != "y push • esc cancel" {
		t.Fatalf("push keys %q", ansi.Strip(push.Keys()))
	}
}

// TestConfirmLines checks the confirm's fit: one line when the
// consequence and keys fit, else the keys on their own line; the question
// and keys are never cut, the consequence only when rows are short.
func TestConfirmLines(t *testing.T) {
	c := NewConfirm(newCommon(), "delete", "delete?", "loses sandbox, storage, unpushed work")
	strip := func(ls []string) []string {
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = ansi.Strip(l)
		}
		return out
	}
	one := strip(c.Lines(72, 0))
	if len(one) != 1 || !strings.HasPrefix(one[0], "loses sandbox, storage, unpushed work") ||
		!strings.HasSuffix(one[0], "y delete • esc keep") || ansi.StringWidth(one[0]) != 72 {
		t.Fatalf("wide %q", one)
	}
	two := strip(c.Lines(42, 0))
	if len(two) != 2 || two[0] != "loses sandbox, storage, unpushed work" ||
		two[1] != strings.Repeat(" ", 42-19)+"y delete • esc keep" {
		t.Fatalf("narrow %q", two)
	}
	wrapped := strip(c.Lines(20, 0))
	if len(wrapped) != 4 || strings.Contains(strings.Join(wrapped, ""), "…") || wrapped[3] != " y delete • esc keep" {
		t.Fatalf("wrapped %q", wrapped)
	}
	// Two lines at most: the consequence is cut, never the keys.
	short := strip(c.Lines(20, 2))
	if len(short) != 2 || !strings.HasSuffix(short[0], "…") || short[1] != " y delete • esc keep" {
		t.Fatalf("two lines %q", short)
	}
	line := ansi.Strip(c.EndLine("▲ a very long session name that does not fit", 30))
	if !strings.HasSuffix(line, "  delete?") || ansi.StringWidth(line) != 30 || !strings.Contains(line, "…") {
		t.Fatalf("end line %q", line)
	}
}

// TestConfirmLooksLikeTheHelp checks the confirm's colours: the question
// in the prompt's question colour, a destructive consequence in the
// danger colour, and the keys exactly as the help footer renders them,
// y in the danger key colour.
func TestConfirmLooksLikeTheHelp(t *testing.T) {
	c := NewConfirm(newCommon(), "delete", "delete?", "loses history")
	st := c.common.Styles
	sep := footer.NewHelp(c.common).Styles.ShortSeparator.Render(" • ")
	answer := st.Action.DangerKey.Render("y") + " " + st.Action.DangerDesc.Render("delete") + sep +
		st.HelpKey.Render("esc") + " " + st.HelpValue.Render("keep")
	if c.Keys() != answer {
		t.Fatalf("keys\n%q\nwant\n%q", c.Keys(), answer)
	}
	want := st.Action.DangerDesc.Render("loses history") + strings.Repeat(" ", 40-13-19) + answer
	if got := c.Lines(40, 0); len(got) != 1 || got[0] != want {
		t.Fatalf("lines\n%q\nwant\n%q", got, want)
	}
	if got := c.EndLine("x", 10); got != "x"+strings.Repeat(" ", 2)+st.Prompt.Question.Render("delete?") {
		t.Fatalf("end line %q", got)
	}
	// The footer, showing the same keys as its help, renders them alike.
	f := footer.New(c.common, keys(c.ShortHelp()))
	if !strings.Contains(f.View(), answer) {
		t.Fatalf("the help footer renders the keys as %q, the confirm as %q", f.View(), answer)
	}
	push := NewConfirm(c.common, "push", "push main → github?", "fast-forward only")
	if want := st.HelpKey.Render("y") + " " + st.HelpValue.Render("push"); !strings.HasPrefix(push.Keys(), want) {
		t.Fatalf("push keys %q", push.Keys())
	}
}

// TestFormStyles checks the form's colours come from the style sheet:
// labels and details as the repository list's descriptions, the focused
// field as its selected title, an error in the prompt's error colour.
func TestFormStyles(t *testing.T) {
	f := newTestForm()
	ps := f.common.Styles.Prompt
	v := f.View(80)
	for _, want := range []string{
		ps.Title.Render("New session"),
		ps.ModeNormal.Render(" NORMAL "),
		ps.Pointer.Render("▸"),
		ps.Label.Width(13).Render("Repository"),
		ps.ActiveLabel.Width(13).Render("Name"),
		ps.ActiveValue.Render("claude-1"),
		ps.Value.Render("sundae"),
		ps.Value.Render("‹ codex ›"),
		ps.Detail.Render("img-codex · codex"),
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q:\n%q", want, v)
		}
	}
	f.Update(press("/"))
	if !strings.Contains(f.View(80), ps.ModeTyping.Render(" TYPING ")) {
		t.Fatalf("no typing badge:\n%q", f.View(80))
	}
	for range len("claude-1") {
		f.Update(press("backspace"))
	}
	f.Update(press("enter"))
	f.Update(press("enter"))
	if want := ps.Error.Render("✗ name is required"); !strings.Contains(f.View(80), want) {
		t.Fatalf("view lacks %q:\n%q", want, f.View(80))
	}
}

// keys is a help.KeyMap of bindings.
type keys []key.Binding

func (k keys) ShortHelp() []key.Binding  { return k }
func (k keys) FullHelp() [][]key.Binding { return [][]key.Binding{k} }

func TestTextFieldCharLimit(t *testing.T) {
	long := strings.Repeat("x", 100)
	for _, tc := range []struct {
		limit int
		want  int
	}{{0, 64}, {256, 100}, {10, 10}} {
		fd := TextField("description", "Description", "", nil)
		if tc.limit > 0 {
			fd.SetCharLimit(tc.limit)
		}
		f := NewForm(newCommon(), "New repository", "create", fd)
		f.StartTyping()
		for _, r := range long {
			f.Update(press(string(r)))
		}
		if got := len(fd.Value()); got != tc.want {
			t.Errorf("limit %d: %d characters, want %d", tc.limit, got, tc.want)
		}
	}
	// Other kinds ignore it.
	ToggleField("t", "T", false).SetCharLimit(1)
}

func TestMultiField(t *testing.T) {
	opts := []Option{{Value: "app"}, {Value: "infra"}, {Value: "lib"}, {Value: "gone"}}
	// Values not among the options are dropped.
	m := MultiField("repos", "Repositories", opts, []string{"lib", "nope"})
	f := NewForm(newCommon(), "Pick", "go", m, ToggleField("t", "T", false))
	var changes int
	f.OnChange = func(*Form, string) { changes++ }
	if m.Kind() != Multi || strings.Join(m.Values(), ",") != "lib" || m.Value() != "lib" {
		t.Fatalf("initial %v", m.Values())
	}
	// Picked first, then the rest; the cursor starts on the top row.
	view := ansi.Strip(f.View(80))
	for _, want := range []string{"▸ Repositories   / to filter", "1 of 4 picked", "▸ [x] lib", "  [ ] app", "  [ ] infra", "  [ ] gone"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in:\n%s", want, view)
		}
	}
	if strings.Index(view, "[x] lib") > strings.Index(view, "[ ] app") {
		t.Fatalf("picked not first:\n%s", view)
	}
	// j moves, space toggles; the cursor follows the option.
	f.Update(press("j"))    // app
	f.Update(press("down")) // infra
	f.Update(press("space"))
	if strings.Join(m.Values(), ",") != "lib,infra" || changes != 1 {
		t.Fatalf("values %v changes %d", m.Values(), changes)
	}
	if o, _ := m.Selected(); o.Value != "infra" {
		t.Fatalf("cursor on %s", o.Value)
	}
	// k moves up.
	f.Update(press("k"))
	if o, _ := m.Selected(); o.Value != "lib" {
		t.Fatalf("cursor on %s", o.Value)
	}
	// In normal mode letters do not filter.
	f.Update(press("a"))
	if m.filter != "" {
		t.Fatalf("normal mode filtered %q", m.filter)
	}
	// / starts typing; picked options hidden by the filter stay, marked.
	f.Update(press("/"))
	for _, k := range []string{"a", "p"} {
		f.Update(press(k))
	}
	if o, ok := m.Selected(); !ok || o.Value != "app" {
		t.Fatalf("filtered %v %v", o, ok)
	}
	view = ansi.Strip(f.View(80))
	for _, want := range []string{"filter: ap█", "TYPING", "[x] lib  (hidden by filter)", "▸ [ ] app"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in:\n%s", want, view)
		}
	}
	if strings.Contains(view, "gone") {
		t.Fatalf("filter shows gone:\n%s", view)
	}
	helps := func() string {
		var s []string
		for _, b := range f.ShortHelp() {
			s = append(s, b.Help().Key+" "+b.Help().Desc)
		}
		return strings.Join(s, ",")
	}
	if h := helps(); !strings.Contains(h, "space pick") || !strings.Contains(h, "enter done") || !strings.Contains(h, "type filter") {
		t.Fatalf("typing help %s", h)
	}
	// backspace edits; enter ends typing and keeps the filter.
	f.Update(press("backspace"))
	if !strings.Contains(ansi.Strip(f.View(80)), "filter: a█") {
		t.Fatalf("backspace:\n%s", ansi.Strip(f.View(80)))
	}
	f.Update(press("enter"))
	if f.Typing() || m.filter != "a" {
		t.Fatalf("enter: typing %v filter %q", f.Typing(), m.filter)
	}
	if h := helps(); !strings.Contains(h, "esc clear filter") || !strings.Contains(h, "j/k move") {
		t.Fatalf("normal help %s", h)
	}
	// esc clears the filter, not the form.
	if res, _ := f.Update(press("esc")); res != Pending || m.filter != "" {
		t.Fatal("esc with a filter cancelled the form")
	}
	// j at the bottom moves to the next field; tab does too, from anywhere.
	for range 4 {
		f.Update(press("j"))
	}
	if f.Focused() == m {
		t.Fatal("j at the bottom stayed")
	}
	// Unfocused, the field is a short summary.
	view = ansi.Strip(f.View(80))
	if !strings.Contains(view, "Repositories   lib, infra") || strings.Contains(view, "[ ]") {
		t.Fatalf("blurred:\n%s", view)
	}
	f.Update(press("k"))
	if f.Focused() != m {
		t.Fatal("k did not return")
	}
	f.Update(press("tab"))
	if f.Focused() == m {
		t.Fatal("tab stayed")
	}
	m.toggle("app", true)
	m.toggle("gone", true)
	if s := m.summary(); s != "lib, infra +2" {
		t.Fatalf("summary %s", s)
	}
	// Nothing picked is refused at submit.
	for _, v := range m.Values() {
		m.toggle(v, false)
	}
	if res, _ := f.Update(press("enter")); res != Pending || f.Err() == nil || !strings.Contains(f.Err().Error(), "pick at least one") {
		t.Fatalf("empty submit: %v", f.Err())
	}
}

// TestMultiFieldScrolls checks a long list shows a bounded window with
// more-indicators that follows the cursor.
func TestMultiFieldScrolls(t *testing.T) {
	var opts []Option
	for i := range 20 {
		opts = append(opts, Option{Value: "repo" + itoa(i)})
	}
	m := MultiField("repos", "Repositories", opts, nil)
	f := NewForm(newCommon(), "Pick", "go", m)
	view := ansi.Strip(f.View(80))
	if !strings.Contains(view, "↓ 13 more") || strings.Contains(view, "↑") || strings.Contains(view, "repo7 ") {
		t.Fatalf("top:\n%s", view)
	}
	for range 10 {
		f.Update(press("down"))
	}
	view = ansi.Strip(f.View(80))
	if !strings.Contains(view, "▸ [ ] repo10") || !strings.Contains(view, "↑ 4 more") || !strings.Contains(view, "↓ 9 more") {
		t.Fatalf("scrolled:\n%s", view)
	}
	if n := strings.Count(view, "[ ]"); n != multiRows {
		t.Fatalf("%d rows:\n%s", n, view)
	}
}
