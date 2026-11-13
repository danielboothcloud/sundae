// Package config implements the `soft config` command.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	sconfig "github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/spf13/cobra"
)

var (
	checkFile string

	// Command is the config command.
	Command = &cobra.Command{
		Use:   "config",
		Short: "Work with the server configuration",
		Args:  cobra.NoArgs,
	}

	checkCmd = &cobra.Command{
		Use:   "check",
		Short: "Check the server configuration file",
		Long: `Check loads the server configuration the way "soft serve" does and reports
every problem it finds: the YAML file (--file, else $SOFT_SERVE_CONFIG_LOCATION,
else config.yaml in the data path), then the SOFT_SERVE_* environment
variables as overrides, then validation. Unknown keys are errors.

It exits 0 when the configuration is valid. It never writes a file: where
"soft serve" would write a default config, check reports the file as missing.
Files the config refers to and that are checked at startup, such as
openshell.token_file when OpenShell is enabled, every forges[].token_file,
notifications.ntfy.token_file when ntfy is enabled and
snapshots.s3.credentials_file when S3 snapshots are enabled, must be readable
here too.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			path := checkPath(checkFile)
			if err := check(path); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "config OK: %s\n", path)
			return nil
		},
	}
)

func init() {
	checkCmd.Flags().StringVarP(&checkFile, "file", "f", "", "config file to check (default $SOFT_SERVE_CONFIG_LOCATION, else config.yaml in the data path)")
	Command.AddCommand(checkCmd)
}

// checkPath returns the config file to check: file when set, else
// $SOFT_SERVE_CONFIG_LOCATION, else the server's default location.
//
// Unlike the server, a SOFT_SERVE_CONFIG_LOCATION that does not exist is not
// replaced by the data path default: it is reported as missing.
func checkPath(file string) string {
	if file != "" {
		return file
	}
	if loc := os.Getenv("SOFT_SERVE_CONFIG_LOCATION"); loc != "" {
		return loc
	}
	return sconfig.DefaultConfig().ConfigPath()
}

// check loads the config file at path with the environment overrides and
// validates it. It returns an error listing every problem found.
func check(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("config file %s does not exist", path)
	case err != nil:
		return fmt.Errorf("config file %s: %w", path, err)
	case info.IsDir():
		return fmt.Errorf("config file %s is a directory", path)
	}

	cfg := sconfig.DefaultConfig()
	if err := cfg.Load(path); err != nil {
		problems := flatten(err)
		noun := "problems"
		if len(problems) == 1 {
			noun = "problem"
		}
		return fmt.Errorf("config file %s: %d %s:\n  %s", path, len(problems), noun, strings.Join(problems, "\n  "))
	}

	return nil
}

// flatten returns the messages of the errors joined in err, one per problem.
func flatten(err error) []string {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var msgs []string
		for _, e := range joined.Unwrap() {
			msgs = append(msgs, flatten(e)...)
		}
		return msgs
	}
	return strings.Split(err.Error(), "\n")
}
