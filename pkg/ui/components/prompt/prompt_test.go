package prompt

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
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
		FixedField("repo", "Repository", "soft-serve"),
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
	for range len("claude-1") {
		f.Update(press("backspace"))
	}
	if !name.Edited() || name.Value() != "" {
		t.Fatalf("value %q edited %v", name.Value(), name.Edited())
	}
	if res, _ := f.Update(press("enter")); res != Pending {
		t.Fatalf("submitted with an empty name: %v", res)
	}
	if f.Err() == nil || !strings.Contains(f.View(80), "name is required") {
		t.Fatalf("error not shown: %v\n%s", f.Err(), f.View(80))
	}
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
	if res, _ := f.Update(press("enter")); res != Pending || f.Err() == nil {
		t.Fatalf("form validation ignored: %v %v", res, f.Err())
	}
	f.Update(press("2"))
	if res, _ := f.Update(press("enter")); res != Submitted {
		t.Fatalf("result %v, err %v", res, f.Err())
	}
	if got := name.Value(); got != "tui2" {
		t.Fatalf("name %q", got)
	}
}

func TestFormFilterableChoice(t *testing.T) {
	opts := []Option{{Value: "dotfiles"}, {Value: "infra"}, {Value: "soft-serve"}, {Value: "scratch"}}
	f := NewForm(newCommon(), "New session", "create", ChoiceField("repo", "Repository", opts, "", true))
	repo := f.Field("repo")
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
	if res, _ := f.Update(press("enter")); res != Pending || f.Err() == nil {
		t.Fatal("submitted without a choice")
	}
	f.Update(press("backspace"))
	f.Update(press("backspace"))
	f.Update(press("right"))
	if got := repo.Value(); got != "dotfiles" {
		t.Fatalf("after s, right: %q (wraps from scratch)", got)
	}
	if res, _ := f.Update(press("esc")); res != Pending {
		t.Fatal("esc with a filter cancelled the form")
	}
	if res, _ := f.Update(press("esc")); res != Cancelled {
		t.Fatal("esc without a filter did not cancel")
	}
}

func TestFormView(t *testing.T) {
	got := ansi.Strip(newTestForm().View(80))
	want := strings.Join([]string{
		"New session",
		"",
		"  Repository   soft-serve",
		"  Name         claude-1 ",
		"  Profile      ‹ codex ›   img-codex · codex",
		"  Attach       yes",
	}, "\n")
	if got != want {
		t.Fatalf("view:\n%s\nwant:\n%s", got, want)
	}
}

func TestConfirm(t *testing.T) {
	c := NewConfirm(newCommon(), "End infra / claude-1?  The sandbox and any unpushed work are deleted.")
	if c.Update(press("y")) != Submitted {
		t.Fatal("y did not confirm")
	}
	for _, k := range []string{"n", "N", "enter", "esc", "x"} {
		if c.Update(press(k)) != Cancelled {
			t.Fatalf("%s did not cancel", k)
		}
	}
	if c.Update(tea.WindowSizeMsg{}) != Pending {
		t.Fatal("a non-key message closed the confirm")
	}
	v := ansi.Strip(c.View(80))
	if !strings.HasSuffix(v, "y / N") || ansi.StringWidth(v) != 80 {
		t.Fatalf("view %q (%d)", v, ansi.StringWidth(v))
	}
	v = ansi.Strip(c.View(40))
	if ansi.StringWidth(v) > 40 || !strings.HasSuffix(v, "y / N") || !strings.Contains(v, "…") {
		t.Fatalf("narrow view %q", v)
	}
}
