package cmd

import (
	"time"

	"charm.land/lipgloss/v2/table"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

// sessionSnapshotCommand groups the commands that manage the S3 snapshots
// of your sessions.
func sessionSnapshotCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snapshot",
		Aliases: []string{"snapshots"},
		Short:   "List and delete the S3 snapshots of your sessions",
		Long: `List and delete the S3 snapshots of your sessions (snapshots.s3).

A session's snapshot is kept when the session ends, and starting a session
of the same name restores it. Nothing deletes snapshots on its own: use
"session snapshot rm", "session snapshot prune", or "session end
--delete-snapshot".`,
	}
	cmd.AddCommand(sessionSnapshotListCommand(), sessionSnapshotRmCommand(), sessionSnapshotPruneCommand())
	return cmd
}

// withManager runs fn with a session Manager on a new gateway client.
func withManager(cmd *cobra.Command, fn func(*openshell.Manager) error) error {
	ctx := cmd.Context()
	client, err := newOpenShellClient(config.FromContext(ctx))
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck
	return fn(newManager(ctx, client))
}

func sessionSnapshotListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List your sessions' snapshots",
		Long: `List your sessions' S3 snapshots, newest first, with what became of
each one's session: live (it has a sandbox), parked (parked to this
snapshot, its only copy of /sandbox) or ended.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd, func(m *openshell.Manager) error {
				snaps, err := m.Snapshots(cmd.Context(), proto.UserFromContext(cmd.Context()))
				if err != nil {
					return err
				}
				if asJSON {
					if snaps == nil {
						snaps = []openshell.StoredSnapshot{}
					}
					return writeJSON(cmd, snaps)
				}
				if len(snaps) == 0 {
					cmd.Println("No snapshots found")
					return nil
				}
				t := table.New().Headers("Session", "State", "Size", "Stored", "URL")
				for _, s := range snaps {
					t = t.Row(s.Label(), s.State, humanize.IBytes(uint64(max(s.Size, 0))), humanize.RelTime(s.Time, time.Now(), "ago", "from now"), s.URL)
				}
				cmd.Println(t)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func sessionSnapshotRmCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm SESSION",
		Aliases: []string{"delete"},
		Short:   "Delete a session's snapshot",
		Long: `Delete the S3 snapshot of one of your sessions. A live session keeps
running; it is snapshotted again when it ends or is parked (with
snapshots on). The snapshot a session is parked to is its only copy of
/sandbox and is not deleted: end the session with
"session end SESSION --delete-snapshot" instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withManager(cmd, func(m *openshell.Manager) error {
				s, err := m.DeleteSnapshot(cmd.Context(), proto.UserFromContext(cmd.Context()), args[0])
				if err != nil {
					return err
				}
				cmd.PrintErrf("Deleted the snapshot of %s (%s)\n", args[0], s.Info().Describe())
				return nil
			})
		},
	}
	return cmd
}

func sessionSnapshotPruneCommand() *cobra.Command {
	var olderThan time.Duration
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete the snapshots of ended sessions",
		Long: `Delete the S3 snapshots of your ended sessions: those with neither a
sandbox nor a parked marker. Snapshots of live and parked sessions are
kept. --older-than keeps the snapshots stored more recently than that
(e.g. 720h for 30 days); --dry-run lists what would be deleted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd, func(m *openshell.Manager) error {
				pruned, err := m.PruneSnapshots(cmd.Context(), proto.UserFromContext(cmd.Context()), openshell.PruneOptions{OlderThan: olderThan, DryRun: dryRun})
				verb := "Deleted"
				if dryRun {
					verb = "Would delete"
				}
				var total int64
				for _, s := range pruned {
					total += s.Size
					cmd.Printf("%s the snapshot of %s (%s)\n", verb, s.Label(), s.Info().Describe())
				}
				if err != nil {
					return err
				}
				if len(pruned) == 0 {
					cmd.Println("No snapshots to prune")
					return nil
				}
				cmd.PrintErrf("%s %d snapshot(s), %s\n", verb, len(pruned), humanize.IBytes(uint64(max(total, 0))))
				return nil
			})
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "only prune snapshots stored longer ago than this (e.g. 720h)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be deleted without deleting it")
	return cmd
}
