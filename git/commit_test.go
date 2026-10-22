package git

import (
	"strings"
	"testing"
)

func TestParseChangeID(t *testing.T) {
	const id = "znkkpsqqskkkvpnoqwmzxqwtnnzlrmow"
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "header present",
			raw:  "tree abc\nparent def\nauthor A <a@b.c> 1 +0000\ncommitter A <a@b.c> 1 +0000\nchange-id " + id + "\n\nmsg\n",
			want: id,
		},
		{
			name: "header missing",
			raw:  "tree abc\nauthor A <a@b.c> 1 +0000\ncommitter A <a@b.c> 1 +0000\n\nmsg\n",
			want: "",
		},
		{
			name: "change-id only in message is ignored",
			raw:  "tree abc\nauthor A <a@b.c> 1 +0000\n\nchange-id " + id + "\n",
			want: "",
		},
		{
			name: "control characters rejected",
			raw:  "tree abc\nchange-id \x1b]0;pwn\x07\n\nmsg\n",
			want: "",
		},
		{
			name: "overlong rejected",
			raw:  "tree abc\nchange-id " + strings.Repeat("a", 65) + "\n\nmsg\n",
			want: "",
		},
		{name: "empty", raw: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseChangeID([]byte(tt.raw)); got != tt.want {
				t.Errorf("ParseChangeID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShortChangeID(t *testing.T) {
	if got := ShortChangeID("znkkpsqqskkkvpnoqwmzxqwtnnzlrmow"); got != "znkkpsqq" {
		t.Errorf("got %q", got)
	}
	if got := ShortChangeID(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestRepositoryChangeID(t *testing.T) {
	repo, _ := setupTestRepo(t)
	c, err := repo.CommitByRevision("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// Plain-git commit: no change-id header.
	if got := repo.ChangeID(c); got != "" {
		t.Errorf("ChangeID() = %q, want empty", got)
	}
}
