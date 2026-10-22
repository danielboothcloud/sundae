package proto

import "testing"

func TestParseVCS(t *testing.T) {
	for in, want := range map[string]VCS{"": VCSJJ, "jj": VCSJJ, "JJ": VCSJJ, "git": VCSGit, " git ": VCSGit} {
		got, err := ParseVCS(in)
		if err != nil || got != want {
			t.Errorf("ParseVCS(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseVCS("hg"); err == nil {
		t.Error("ParseVCS(hg) = nil error, want error")
	}
}

func TestCloneCommand(t *testing.T) {
	const url = "ssh://h/r.git"
	if got := CloneCommand(VCSJJ, url); got != "jj git clone "+url {
		t.Errorf("jj: %q", got)
	}
	if got := CloneCommand(VCSGit, url); got != url {
		t.Errorf("git: %q", got)
	}
}
