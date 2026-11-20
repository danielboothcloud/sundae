package common

import (
	"context"
	"errors"
	"testing"
)

func TestNoticeViewKinds(t *testing.T) {
	c := NewCommon(context.Background(), 80, 24)
	ps := c.Styles.Prompt
	cases := []struct {
		text  string
		kind  MessageKind
		width int
		want  string
	}{
		{"Creating mine", MessageBusy, 80, ps.Notice.Render("Creating mine …")},
		{"Deleting mine …", MessageBusy, 80, ps.Notice.Render("Deleting mine …")},
		{"Kept mine.", MessageInfo, 80, ps.Notice.Render("Kept mine.")},
		{"Created mine.", MessageSuccess, 80, ps.Success.Render("✓ Created mine.")},
		{"mine: unauthorized", MessageError, 80, ps.Error.Render("✗ mine: unauthorized")},
		{"two\nlines", MessageError, 80, ps.Error.Render("✗ two lines")},
		{"Left infra / claude-1; it keeps running.", MessageInfo, 20, ps.Notice.Render("Left infra / claude…")},
	}
	for _, tc := range cases {
		if got := c.NoticeView(tc.text, tc.kind, tc.width); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestShortError(t *testing.T) {
	cases := map[string]string{
		"dial tcp 127.0.0.1:1: connect: connection refused":                         "connection refused",
		`Get "https://api.github.com/user/repos": dial tcp: lookup x: no such host`: "host not found",
		"rpc error: code = Unavailable desc = context deadline exceeded":            "timed out",
		"forge github: GET /user/repos: 401 Unauthorized: Bad credentials":          "forge github: 401 Unauthorized: Bad credentials",
		"repository already\nexists":                                                "repository already exists",
	}
	for in, want := range cases {
		if got := ShortError(errors.New(in)); got != want {
			t.Errorf("ShortError(%q) = %q, want %q", in, got, want)
		}
	}
	if ShortError(nil) != "" {
		t.Error("ShortError(nil) is not empty")
	}
}
