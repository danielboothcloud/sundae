package cmd

import (
	"strings"

	"charm.land/lipgloss/v2/table"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/spf13/cobra"
)

// OpenShellCommand returns the command that shows how Sundae manages the
// OpenShell gateway. It is only registered when OpenShell is enabled, and
// is for server admins: it shows server-wide state.
func OpenShellCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "openshell",
		Short:             "Show the OpenShell providers Sundae manages (admins)",
		PersistentPreRunE: checkIfServerAdmin,
	}
	cmd.AddCommand(openshellProvidersCommand())
	return cmd
}

func openshellProvidersCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List the OpenShell providers: declared, created by Sundae, or attached by name",
		Long: `List the OpenShell providers Sundae knows of:

  - those declared in openshell.providers, which Sundae creates and keeps up
    to date on the gateway from their credential files: whether Sundae
    created them (managed) or took them over (adopted), the last sync and
    its error, and the names of their credentials (never the values);
  - those Sundae created that are no longer declared (undeclared), kept
    unless openshell.prune_providers is set;
  - those a profile lists that are not declared (manual): Sundae attaches
    them by name and does not manage them.

A session start fails when its profile lists a declared provider whose
last sync failed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			report := openshell.ProviderReport(config.FromContext(ctx), openshell.ProviderReconcilerFromContext(ctx))
			if asJSON {
				if report == nil {
					report = []openshell.ProviderStatus{}
				}
				return writeJSON(cmd, report)
			}
			if len(report) == 0 {
				cmd.Println("No OpenShell providers are declared (openshell.providers) or listed by a profile.")
				return nil
			}
			t := table.New().Headers("Provider", "Type", "State", "Owner", "Last sync", "Credentials", "Profiles", "Last error")
			for _, st := range report {
				typ := st.Type
				if typ == "" {
					typ = "-"
				}
				t = t.Row(st.Name, typ, st.State, providerOwner(st), ago(st.LastSync),
					dashJoin(st.CredentialKeys), dashJoin(st.Profiles), st.LastError)
			}
			cmd.Println(t)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// providerOwner says who manages a provider: sundae (created it), adopted,
// manual (not declared) or other (declared, but someone else's).
func providerOwner(st openshell.ProviderStatus) string {
	switch {
	case st.Managed:
		return "sundae"
	case st.Adopted:
		return "adopted"
	case !st.Declared:
		return "manual"
	case st.State == openshell.ProviderPending:
		return "-"
	default:
		return "other"
	}
}

// dashJoin joins a list with commas, or "-" for none.
func dashJoin(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ",")
}
