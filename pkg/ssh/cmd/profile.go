package cmd

import (
	"errors"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/spf13/cobra"
)

// profileCommand sets or gets a repository's default session profile.
func profileCommand() *cobra.Command {
	var unset bool
	cmd := &cobra.Command{
		Use:   "profile REPOSITORY [PROFILE]",
		Short: "Set or get the default session profile of a repository",
		Long: `Set or get the session profile new agent sessions of a repository start
from when "session start" is given no --profile. It must be one of
sessions.profiles in the server's config, and is kept in the repository's
git config. --unset removes it.

A new session's profile is the first of: session start --profile, this
setting, the sessions.repos pattern matching the repository, none.`,
		Args:              cobra.RangeArgs(1, 2),
		PersistentPreRunE: checkIfReadable,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			rn := strings.TrimSuffix(args[0], ".git")

			switch {
			case unset && len(args) == 2:
				return errors.New("--unset takes no PROFILE")
			case unset, len(args) == 2:
				if err := checkIfRepoCollab(cmd, args); err != nil {
					return err
				}
				profile := ""
				if !unset {
					profile = args[1]
				}
				return be.SetRepoProfile(ctx, rn, profile)
			default:
				profile, err := be.RepoProfile(ctx, rn)
				if err != nil {
					return err
				}
				cmd.Println(profile)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&unset, "unset", false, "remove the repository's default session profile")

	return cmd
}
