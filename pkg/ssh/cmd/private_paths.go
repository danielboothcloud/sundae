package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/spf13/cobra"
)

// privatePathsCommand sets or gets the private paths of a forge-linked
// repository.
func privatePathsCommand() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "private-paths REPOSITORY [PATH...]",
		Short: "Set or get the Sundae-only paths of a forge-linked repository",
		Long: `Set or get the private paths of a forge-linked repository: files, or
directories, that Sundae keeps on refs/sundae/private and the forge never
receives. Sessions restore them and save them on suspend, delete and
"session save-private"; a forwarded push that adds one is refused.

With no PATH, print the effective list and where it comes from: the
repository's own list ("repo"), else the forge's private_paths ("forge").
PATHs replace the repository's list; --clear removes it, so the forge's
applies again. The list is kept in the repository's git config
(sundae.privatepath).`,
		Args:              cobra.MinimumNArgs(1),
		PersistentPreRunE: checkIfReadable,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg := config.FromContext(ctx)
			rn := strings.TrimSuffix(args[0], ".git")
			rr, err := backend.FromContext(ctx).Repository(ctx, rn)
			if err != nil {
				return err
			}
			notLinked := func(err error) error {
				if errors.Is(err, forge.ErrNotLinked) {
					return fmt.Errorf("%s is not linked to a configured forge; private paths apply to forge-linked repositories only", rr.Name())
				}
				return err
			}

			switch {
			case clear && len(args) > 1:
				return errors.New("--clear takes no PATH")
			case clear, len(args) > 1:
				if err := checkIfRepoCollab(cmd, args); err != nil {
					return err
				}
				return notLinked(forge.SetPrivatePaths(ctx, cfg, rr, args[1:]))
			}
			pp, err := forge.PrivatePathsOf(ctx, cfg, rr)
			if err != nil {
				return notLinked(err)
			}
			cmd.Println("Source:", pp.Source)
			if len(pp.Paths) > 0 {
				cmd.Println("Paths:")
				for _, p := range pp.Paths {
					cmd.Println("  -", p)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the repository's list, so the forge's private_paths apply")
	return cmd
}
