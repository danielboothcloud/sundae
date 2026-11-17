package common

import (
	"context"
	"testing"
)

func TestNoticeView(t *testing.T) {
	c := NewCommon(context.Background(), 80, 24)
	ps := c.Styles.Prompt
	cases := []struct {
		text  string
		isErr bool
		want  string
	}{
		{"Started infra / claude-1.", false, ps.Success.Render("✓ Started infra / claude-1.")},
		{"Deleted mine.", false, ps.Success.Render("✓ Deleted mine.")},
		{"Cancelled.", false, ps.Notice.Render("Cancelled.")},
		{"infra is already parked.", false, ps.Notice.Render("infra is already parked.")},
		{"cannot list", true, ps.Error.Render("✗ cannot list")},
		{"✗ scratch: unauthorized", false, ps.Error.Render("✗ scratch: unauthorized")},
		{"✓ done already", false, ps.Success.Render("✓ done already")},
	}
	for _, tc := range cases {
		if got := c.NoticeView(tc.text, tc.isErr, 80); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.text, got, tc.want)
		}
	}
}
