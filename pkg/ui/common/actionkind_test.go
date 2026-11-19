package common

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/key"
)

func TestKindOf(t *testing.T) {
	b := func(k, desc string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, desc)) }
	cases := []struct {
		b    key.Binding
		want ActionKind
	}{
		{b("enter", "attach"), ActionPrimary},
		{b("d", "delete"), ActionDanger},
		{b("x", "Delete repository"), ActionDanger},
		{b("R", "remove repos"), ActionDanger},
		{b("enter", "delete session"), ActionDanger}, // danger wins
		{b("z", "suspend"), ActionNormal},
		{b("n", "new"), ActionNormal},
		{b("e", "endpoint"), ActionNormal}, // a word, not a prefix
	}
	for _, tc := range cases {
		if got := KindOf(tc.b); got != tc.want {
			t.Errorf("%s %q: %v, want %v", tc.b.Help().Key, tc.b.Help().Desc, got, tc.want)
		}
	}
	c := NewCommon(context.Background(), 80, 24)
	if got, want := c.KeyStyle(b("d", "delete")).Render("d"), c.Styles.Action.DangerKey.Render("d"); got != want {
		t.Errorf("danger key %q, want %q", got, want)
	}
	if got, want := c.KeyStyle(b("enter", "attach")).Render("enter"), c.Styles.Action.PrimaryKey.Render("enter"); got != want {
		t.Errorf("primary key %q, want %q", got, want)
	}
	if got, want := c.KeyStyle(b("n", "new")).Render("n"), c.Styles.HelpKey.Render("n"); got != want {
		t.Errorf("normal key %q, want %q", got, want)
	}
}
