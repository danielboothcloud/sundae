package ssh

import (
	"os"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/jjview"
)

// TestMain turns the jj view off: the UI tests show jj repositories with
// the git log, and no jj runs in the background past a test's end.
// pkg/jjview tests the jj view.
func TestMain(m *testing.M) {
	os.Setenv(jjview.EnvJJPath, "off") //nolint:errcheck,usetesting
	os.Exit(m.Run())
}
