package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// commitmentCommands is the `commitment` group: what an asker does with an
// advertised evidence commitment before it asks or pays.
func commitmentCommands() *cobra.Command {
	group := &cobra.Command{Use: "commitment", Short: "Find and check an advertised evidence commitment from a carrier"}
	find := &cobra.Command{Use: "find", Short: "Find an advertised evidence commitment from a directory record, an A2A card or a Nostr relay, and check it", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		var in findInput
		in.Directories, _ = c.Flags().GetStringArray("directory")
		in.Cards, _ = c.Flags().GetStringArray("a2a-card")
		in.Relays, _ = c.Flags().GetStringArray("nostr-relay")
		in.Files, _ = c.Flags().GetStringArray("file")
		in.NodeID, _ = c.Flags().GetString("node-id")
		in.NodeKey, _ = c.Flags().GetString("node-key")
		in.RefuseUnwitnessed, _ = c.Flags().GetBool("refuse-unwitnessed")
		in.Offline, _ = c.Flags().GetBool("offline")
		in.AllowPrivate, _ = c.Flags().GetBool("allow-private-network")
		noPin, _ := c.Flags().GetBool("no-pin")
		in.PinDir, _ = c.Flags().GetString("pin-dir")
		switch {
		case len(in.Directories)+len(in.Cards)+len(in.Relays)+len(in.Files) == 0:
			return InputError("give where the commitment was advertised: --directory, --a2a-card or --nostr-relay with --node-id, or --file for octets you hold")
		case len(in.Relays) > 0 && in.NodeID == "":
			return InputError("--nostr-relay needs --node-id: a relay is searched for one node's announcement")
		case in.NodeID != "" && !lowerHex64.MatchString(in.NodeID):
			return InputError("--node-id is 64 lowercase hex characters")
		case in.NodeKey != "" && !lowerHex64.MatchString(in.NodeKey):
			return InputError("--node-key is 64 lowercase hex characters")
		case noPin && c.Flags().Changed("pin-dir"):
			return InputError("--no-pin and --pin-dir contradict each other")
		}
		var err error
		if _, in.CheckpointKeys, err = stapleKeys(c); err != nil {
			return err
		}
		if in.Receipts, in.Policy, err = loadWitnessInputs(c); err != nil {
			return err
		}
		switch {
		case noPin:
			in.PinDir = ""
		case in.PinDir == "":
			if in.PinDir, err = DefaultPinDir(); err != nil {
				return err
			}
		}
		if len(in.Relays) > maxRelays {
			return InputError("give at most 8 --nostr-relay")
		}
		for _, relay := range in.Relays {
			if !strings.HasPrefix(relay, "wss://") && !(nostrDialInsecure && strings.HasPrefix(relay, "ws://")) {
				return InputError("--nostr-relay is a wss:// URL")
			}
		}
		rep, err := runFind(in)
		if err != nil {
			return err
		}
		if err := Output(c, rep); err != nil {
			return err
		}
		switch rep.Verdict {
		case verdictVerified:
			return nil
		case verdictKeyUnbound:
			// Nothing failed; only the key is left to give, as for verify --history.
			return ErrPartial
		}
		return ErrHistoryNotAccepted
	}}
	f := find.Flags()
	f.StringArray("directory", nil, "An ARD file or NANDA Index record URL (https:) that lists the node's commitment (repeat for each)")
	f.StringArray("a2a-card", nil, "An A2A Agent Card URL (https:) whose commitment extension points to it (repeat for each)")
	f.StringArray("nostr-relay", nil, "A relay (wss:) to search for the node's announce event; needs --node-id (repeat for each)")
	f.String("node-id", "", "The node's id, 64 lowercase hex: the Nostr search key, and a filter on what the other carriers return")
	f.StringArray("file", nil, "Commitment octets you already hold (a file)")
	f.String("node-key", "", "The key you hold for the node, 64 lowercase hex, from the deal or request you hold, never from a carrier; the commitment must be under it. Without it nothing proves the commitment is the node's, and the verdict is KEY_NOT_BOUND")
	f.StringArray("checkpoint-key", nil, "A log key you accept for the stapled cards' checkpoints, 64 lowercase hex (repeat for each); the log key need not be the node's")
	f.StringArray("receipt", nil, "A witness receipt (a COSE Receipt file) over a stapled card's anchor (repeat for each); with none the node is unwitnessed")
	f.String("anchor-witness-policy", "", "The witnesses you accept, JSON {\"witnesses\":[{\"name\",\"keys\":[hex DER SubjectPublicKeyInfo]}]}; a receipt by any other witness counts for nothing")
	f.String("pin-dir", "", "Where your first-seen checkpoints are kept per node key (default: beside your profiles)")
	f.Bool("no-pin", false, "Keep no first-seen checkpoint and check none")
	f.Bool("refuse-unwitnessed", false, "Refuse a node that no witness in your policy vouches for")
	f.Bool("allow-private-network", false, "Contact loopback, private and link-local hosts: by default the hosts a carrier or commitment names must be public")
	f.Bool("offline", false, "Do not ask the node: check only the chain stapled in the commitment")
	group.AddCommand(find)
	return group
}
