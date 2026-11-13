package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/soft-serve/pkg/forge"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// errForgesUnavailable is returned when the forge service is not running,
// which only happens when the command is reached without configured forges.
var errForgesUnavailable = errors.New("no forges are configured")

// ForgeCommand returns the command that works with the GitHub and GitLab
// forges declared in the config file. It is only registered when at least
// one forge is configured.
//
// Listing, importing and syncing are for server admins: they use the
// server's forge tokens and see every repository those tokens can. Pushing
// a repository needs write access to it.
func ForgeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forge",
		Short: "Discover, import, sync and push GitHub and GitLab repositories",
		Long: `Work with the forges (GitHub, GitLab) declared in the server config.

Sync never rewrites or deletes a Soft Serve branch. Upstream branches and tags
are fetched into refs/remotes/<forge>/heads/* and refs/remotes/<forge>/tags/*;
a local branch strictly behind its upstream is fast-forwarded, a missing one
is created, and one that is ahead or has diverged is left alone. Pushing to a
forge is always explicit: forge push.`,
	}

	cmd.AddCommand(
		forgeListCommand(),
		forgeReposCommand(),
		forgeImportCommand(),
		forgeSyncCommand(),
		forgePushCommand(),
	)
	return cmd
}

// forgeService returns the forge service from the command's context.
func forgeService(cmd *cobra.Command) (*forge.Service, error) {
	svc := forge.FromContext(cmd.Context())
	if svc == nil {
		return nil, errForgesUnavailable
	}
	return svc, nil
}

func writeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ago renders a time as its age, or "never".
func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return time.Since(*t).Round(time.Second).String() + " ago"
}

func forgeListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "ls",
		Aliases:           []string{"list"},
		Short:             "List the configured forges",
		Args:              cobra.NoArgs,
		PersistentPreRunE: checkIfServerAdmin,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := forgeService(cmd)
			if err != nil {
				return err
			}
			status, err := svc.Status(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, status)
			}
			t := table.New().Headers("Forge", "Kind", "URL", "Imported", "Listed", "Last sync", "Last error")
			for _, st := range status {
				listed := "-"
				if st.Listed != nil {
					listed = fmt.Sprintf("%d (%d match)", *st.Listed, *st.Matching)
				}
				t = t.Row(st.Name, st.Kind, st.URL, fmt.Sprint(st.Imported), listed, ago(st.LastSync), st.LastError)
			}
			cmd.Println(t)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func forgeReposCommand() *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "repos FORGE",
		Short: "List a forge's repositories, imported or not",
		Long: `List the repositories the forge's token can access that pass the forge's
include/exclude patterns, whether imported or not, plus imported repositories
the forge no longer lists. Ahead/behind compare the local default branch with
the upstream one as of the last sync. --all also lists the repositories the
patterns exclude.`,
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: checkIfServerAdmin,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := forgeService(cmd)
			if err != nil {
				return err
			}
			repos, err := svc.Repos(cmd.Context(), args[0], all)
			if err != nil {
				return err
			}
			if asJSON {
				if repos == nil {
					repos = []forge.RepoStatus{}
				}
				return writeJSON(cmd, repos)
			}
			if len(repos) == 0 {
				cmd.Println("No repositories found")
				return nil
			}
			t := table.New().Headers("Path", "Repository", "State", "Default", "Ahead/behind", "Last sync", "Last error")
			for _, r := range repos {
				state := "available"
				switch {
				case r.Imported && !r.Listed:
					state = "imported, not listed"
				case r.Imported:
					state = "imported"
				case r.Excluded:
					state = "excluded"
				}
				if r.Archived {
					state += ", archived"
				}
				ab := "-"
				if r.Ahead != nil {
					ab = fmt.Sprintf("+%d/-%d", *r.Ahead, *r.Behind)
				}
				last := "-"
				if r.Imported {
					last = ago(r.LastSync)
				}
				t = t.Row(r.Path, r.Repo, state, r.DefaultBranch, ab, last, r.LastError)
			}
			cmd.Println(t)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "also list repositories include/exclude filter out")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func forgeImportCommand() *cobra.Command {
	var all bool
	var vcsFlag string
	cmd := &cobra.Command{
		Use:   "import FORGE [OWNER/REPO...]",
		Short: "Import forge repositories",
		Long: `Import forge repositories as new Soft Serve repositories named
<prefix>/<owner>/<repo> (prefix defaults to the forge name), or with --all
every listed repository the forge's include/exclude patterns let through that
is not imported yet.

A repository private on the forge stays private here. A name already taken by
a repository that is not imported from the same forge path is a collision:
that repository is left alone and the import of it fails.`,
		Args:              cobra.MinimumNArgs(1),
		PersistentPreRunE: checkIfServerAdmin,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) > 1) {
				return errors.New("give either repository paths or --all")
			}
			vcs, err := proto.ParseVCS(vcsFlag)
			if err != nil {
				return err
			}
			svc, err := forgeService(cmd)
			if err != nil {
				return err
			}
			results, err := svc.Import(cmd.Context(), proto.UserFromContext(cmd.Context()), args[0], forge.ImportOptions{
				Paths: args[1:],
				All:   all,
				Vcs:   vcs,
			})
			if err != nil {
				return err
			}
			if len(results) == 0 {
				cmd.Println("Nothing to import")
				return nil
			}
			failed := 0
			for _, r := range results {
				switch r.Status {
				case forge.ImportImported:
					cmd.Printf("imported %s as %s (%s)\n", r.Path, r.Repo, syncSummary(r.Sync))
				case forge.ImportExists:
					cmd.Printf("%s is already imported as %s\n", r.Path, r.Repo)
				default:
					failed++
					cmd.PrintErrf("failed to import %s: %s\n", r.Path, r.Error)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d imports failed", failed, len(results))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "import every listed repository the include/exclude patterns let through")
	cmd.Flags().StringVar(&vcsFlag, "vcs", string(proto.DefaultVCS), "VCS kind of the new repositories (git or jj)")
	return cmd
}

func forgeSyncCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync [FORGE [REPOSITORY]]",
		Short: "Sync imported repositories now",
		Long: `Sync now: every forge, one forge, or one repository of a forge (by its Soft
Serve name or its forge path). Local branches are only created or
fast-forwarded; see "forge --help".`,
		Args:              cobra.RangeArgs(0, 2),
		PersistentPreRunE: checkIfServerAdmin,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := forgeService(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			var results []forge.RepoSyncResult
			switch len(args) {
			case 2:
				r, err := svc.SyncRepo(ctx, args[0], args[1])
				if err != nil {
					return err
				}
				results = append(results, r)
			case 1:
				if results, err = svc.SyncForge(ctx, args[0]); err != nil {
					return err
				}
			default:
				var errs []error
				for _, f := range svc.Forges() {
					rs, err := svc.SyncForge(ctx, f.Name())
					if err != nil {
						errs = append(errs, err)
					}
					results = append(results, rs...)
				}
				if err := errors.Join(errs...); err != nil {
					printSyncResults(cmd, results)
					return err
				}
			}
			return printSyncResults(cmd, results)
		},
	}
	return cmd
}

// printSyncResults prints one line per repository and returns an error when
// any failed.
func printSyncResults(cmd *cobra.Command, results []forge.RepoSyncResult) error {
	if len(results) == 0 {
		cmd.Println("No imported repositories to sync")
		return nil
	}
	failed := 0
	for _, r := range results {
		if r.Error != "" {
			failed++
			cmd.PrintErrf("%s: sync failed: %s\n", r.Repo, r.Error)
			continue
		}
		cmd.Printf("%s: %s\n", r.Repo, syncSummary(r.Sync))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d repositories failed to sync", failed, len(results))
	}
	return nil
}

// syncSummary describes a sync result in a few words; see
// forge.SyncResult.Summary.
func syncSummary(r *forge.SyncResult) string { return r.Summary() }

func forgePushCommand() *cobra.Command {
	var lease bool
	cmd := &cobra.Command{
		Use:   "push REPOSITORY [BRANCH]",
		Short: "Push a branch of an imported repository to its forge",
		Long: `Push a branch (default: the branch HEAD points at) of an imported repository
to its forge, as a fast-forward. When the forge has commits the branch lacks,
the push is refused and the missing commits are reported: sync, merge, and
push again.

--force-with-lease overwrites the forge branch, but only if it is still where
the last sync saw it (refs/remotes/<forge>/heads/<branch>).`,
		Args:              cobra.RangeArgs(1, 2),
		PersistentPreRunE: checkIfReadableAndCollab,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := forgeService(cmd)
			if err != nil {
				return err
			}
			branch := ""
			if len(args) > 1 {
				branch = args[1]
			}
			res, err := svc.Push(cmd.Context(), repoArg(args), branch, lease)
			if err != nil {
				return err
			}
			switch {
			case res.UpToDate:
				cmd.Printf("%s is already up to date on the forge\n", res.Branch)
			case res.Forced:
				cmd.Printf("pushed %s: %s...%s (forced, with lease)\n", res.Branch, short(res.Old), short(res.New))
			default:
				cmd.Printf("pushed %s: %s..%s\n", res.Branch, short(res.Old), short(res.New))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&lease, "force-with-lease", false, "overwrite the forge branch if it is still where the last sync saw it")
	return cmd
}

// short abbreviates an object id; "" (a ref that did not exist) is shown
// as "(new)".
func short(oid string) string {
	switch {
	case oid == "":
		return "(new)"
	case len(oid) > 7:
		return oid[:7]
	default:
		return oid
	}
}
