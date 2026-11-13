// Package kube implements the hidden `sundae kube` commands the Helm chart
// runs in hook Jobs.
package kube

import (
	"context"
	"fmt"
	"time"

	"github.com/danielboothcloud/sundae/pkg/kube"
	"github.com/spf13/cobra"
)

var (
	secretName  string
	secretKey   string
	randomBytes int
	labels      map[string]string
	annotations map[string]string

	// Command is the kube command.
	Command = &cobra.Command{
		Use:    "kube",
		Short:  "Kubernetes helpers for the Helm chart",
		Hidden: true,
		Args:   cobra.NoArgs,
	}

	ensureSecretCmd = &cobra.Command{
		Use:   "ensure-secret",
		Short: "Create a Secret holding a random key, unless it exists",
		Long: `Ensure-secret creates the Secret --name in the pod's namespace, with --key
set to --bytes random bytes, base64-encoded (like "openssl rand -base64 32"),
unless the Secret already exists. An existing Secret is never changed; one
without --key is an error. It runs in a pod, as its service account, which
needs get and create on Secrets.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if secretName == "" || secretKey == "" {
				return fmt.Errorf("--name and --key are required")
			}
			if randomBytes < 16 {
				return fmt.Errorf("--bytes must be at least 16")
			}
			client, ns, err := kube.InCluster()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
			defer cancel()
			tmpl := kube.Secret{Metadata: kube.ObjectMeta{Labels: labels, Annotations: annotations}}
			created, err := client.EnsureSecret(ctx, ns, secretName, secretKey, tmpl, func() ([]byte, error) {
				return kube.RandomBase64(randomBytes)
			})
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintf(c.OutOrStdout(), "created secret %s/%s\n", ns, secretName)
			} else {
				fmt.Fprintf(c.OutOrStdout(), "secret %s/%s exists; left unchanged\n", ns, secretName)
			}
			return nil
		},
	}
)

func init() {
	f := ensureSecretCmd.Flags()
	f.StringVar(&secretName, "name", "", "name of the Secret")
	f.StringVar(&secretKey, "key", "", "key in the Secret that holds the random value")
	f.IntVar(&randomBytes, "bytes", 32, "number of random bytes, before base64 encoding")
	f.StringToStringVar(&labels, "label", nil, "label for a created Secret (key=value, repeatable)")
	f.StringToStringVar(&annotations, "annotation", nil, "annotation for a created Secret (key=value, repeatable)")
	Command.AddCommand(ensureSecretCmd)
}
