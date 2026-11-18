package cli

import (
	"encoding/hex"
	"errors"
	"os"

	"github.com/spf13/cobra"
)

// mailboxCommands is the `mailbox` group: the Nostr mailbox carriage of an
// evidence request (agent-action-capsule spec/bindings/nostr-mailbox-v0.md).
func mailboxCommands() *cobra.Command {
	group := &cobra.Command{Use: "mailbox", Short: "Carry evidence requests and answers over a Nostr mailbox a commitment names"}
	group.AddCommand(mailboxKeyCommand(), mailboxAskCommand(), mailboxPollCommand(), mailboxInboxCommand(), mailboxReplyCommand())
	return group
}

func mailboxKeyCommand() *cobra.Command {
	key := &cobra.Command{Use: "key", Short: "The mailbox key: the secp256k1 key a responder is addressed by", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error { return c.Help() }}
	newKey := &cobra.Command{Use: "new", Short: "Make a mailbox key and print its public key; the secret goes to a file only you can read", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		out, _ := c.Flags().GetString("output")
		if out == "" {
			return InputError("--output is required: the file the secret is written to (mode 600)")
		}
		secret, err := newNostrKey()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return InputError(out + " exists: a mailbox key is never overwritten")
		} else if err != nil {
			return err
		}
		if _, err := f.WriteString(hex.EncodeToString(secret.Serialize()) + "\n"); err != nil {
			f.Close()
			_ = os.Remove(out)
			return err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(out)
			return err
		}
		return Output(c, map[string]string{"mailbox_key": nostrPublicKey(secret), "secret_file": out})
	}}
	newKey.Flags().String("output", "", "The file the secret is written to, mode 600; an existing file is never overwritten")
	key.AddCommand(newKey)
	return key
}
