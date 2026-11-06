package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/sink/webhook"
)

// webhookSecretBytes is the size of a generated signing key.
const webhookSecretBytes = 32

func newWebhookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Helpers for webhook sinks",
	}
	secret := &cobra.Command{
		Use:   "secret",
		Short: "Manage webhook signing secrets",
	}
	secret.AddCommand(&cobra.Command{
		Use:   "new",
		Short: "Print a new webhook signing secret (whsec_ + base64 of 32 random bytes)",
		Long: "Print a new Standard Webhooks signing secret on stdout. It is generated locally " +
			"and is not stored or sent anywhere: put it in the variable named by the sink's " +
			"secret_env, and give it to the receiver.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newWebhookSecret()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), s)
			return err
		},
	})
	cmd.AddCommand(secret)
	return cmd
}

// newWebhookSecret returns "whsec_" + base64 of 32 bytes from crypto/rand.
func newWebhookSecret() (string, error) {
	b := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return webhook.SecretPrefix + base64.StdEncoding.EncodeToString(b), nil
}
