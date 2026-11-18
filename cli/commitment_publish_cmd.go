package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// publishReport is what `commitment publish` prints.
type publishReport struct {
	NodeID string `json:"node_id"`
	KeyID  string `json:"key_id"`
	Stream string `json:"stream"`
	// Digest is the SHA-256 of the commitment octets as written, the digest
	// every carrier entry names.
	Digest string `json:"commitment_digest"`
	// Written are the draft files, always written under --out.
	Written []string `json:"written"`
	// Submitted is empty for a draft: nothing leaves the machine without the
	// seller's yes.
	Submitted map[string]any `json:"submitted"`
	Draft     bool           `json:"draft"`
}

var publishCarriers = []string{"ard", "a2a", "nostr"}

func commitmentPublishCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "publish", Short: "Sign an evidence commitment under the profile's key with its history card (history_card/1) stapled, and, after the seller's yes, hand it to the carriers the seller names", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		f := c.Flags()
		in := publishInput{Now: nostrNow()}
		staplePath, _ := f.GetString("staple")
		out, _ := f.GetString("out")
		yes, _ := f.GetBool("yes")
		to, _ := f.GetStringSlice("to")
		ardFile, _ := f.GetString("ard-file")
		a2aFile, _ := f.GetString("a2a-card")
		ardEntryPath, _ := f.GetString("ard-entry")
		mailboxPath, _ := f.GetString("mailbox-key")
		in.CommitmentURI, _ = f.GetString("commitment-uri")
		in.Community, _ = f.GetString("community")
		in.NodeID, _ = f.GetString("node-id")
		in.Endpoint, _ = f.GetString("endpoint")
		in.Bindings, _ = f.GetStringSlice("binding")
		in.Subjects, _ = f.GetStringSlice("subject")
		in.Derivs, _ = f.GetStringSlice("derivation")
		in.PredicateClasses, _ = f.GetStringSlice("predicate-class")
		in.HumanGated, _ = f.GetBool("human-gated")
		in.AnswersWithin, _ = f.GetString("answers-within")
		in.ShareDealHalf, _ = f.GetString("share-deal-half")
		in.Relays, _ = f.GetStringArray("nostr-relay")
		in.CheckpointKey, _ = f.GetString("checkpoint-key")
		in.AnnounceURIOnly, _ = f.GetBool("nostr-uri-only")

		// The seller's yes comes first: a carrier named without it is refused
		// before a key is read or a file written.
		for _, carrier := range to {
			if !slices.Contains(publishCarriers, carrier) {
				return InputError("--to names ard, a2a or nostr")
			}
		}
		switch {
		case len(to) > 0 && !yes:
			return InputError("--to submits the commitment to the carriers it names, and only the seller's yes lets it: add --yes once the seller has agreed, or leave --to out for a draft")
		case yes && len(to) == 0:
			return InputError("--yes says the seller agreed to submit; name the carriers with --to ard, a2a or nostr")
		case staplePath == "" || out == "":
			return InputError("--staple and --out are required: the History card to staple, and the directory the draft is written to")
		case slices.Contains(to, "ard") && (ardFile == "" || ardEntryPath == "" || in.CommitmentURI == ""):
			return InputError("--to ard needs --ard-file (the seller's own ard.json), --ard-entry (the seller's listing) and --commitment-uri")
		case slices.Contains(to, "a2a") && (a2aFile == "" || in.CommitmentURI == ""):
			return InputError("--to a2a needs --a2a-card (the card file to add the extension to) and --commitment-uri")
		case slices.Contains(to, "nostr") && mailboxPath == "":
			return InputError("--to nostr needs --mailbox-key and --nostr-relay: the announce event is signed by the mailbox key and goes to its relays")
		case in.CommitmentURI != "" && !strings.HasPrefix(in.CommitmentURI, "https://"):
			return InputError("--commitment-uri is an https: URI")
		case in.AnnounceURIOnly && in.CommitmentURI == "":
			return InputError("--nostr-uri-only needs --commitment-uri: the event then points to the octets")
		}

		p, err := selectedProfile(c)
		if err != nil {
			return err
		}
		if p.Signing == (Secret{}) {
			return InputError("profile " + p.Name + " has no signing key: a commitment is signed by the profile's own key")
		}
		if in.Seed, err = PrivateKey(p.Signing); err != nil {
			return err
		}
		if in.CheckpointKey == "" && p.Checkpoint.Signing != (Secret{}) {
			ck, err := PrivateKey(p.Checkpoint.Signing)
			if err != nil {
				return err
			}
			in.CheckpointKey = hex.EncodeToString(ck.Public().(ed25519.PublicKey))
		}
		if in.Staple, err = ReadInput(staplePath); err != nil {
			return err
		}
		if mailboxPath != "" {
			if in.MailboxKey, err = readNostrKey(mailboxPath); err != nil {
				return err
			}
		}
		if ardEntryPath != "" {
			raw, err := ReadInput(ardEntryPath)
			if err != nil {
				return err
			}
			if in.ARDEntry, err = decodeObject(raw); err != nil {
				return InputError("--ard-entry is a JSON object: the seller's own listing entry")
			}
			if in.CommitmentURI == "" {
				return InputError("--ard-entry needs --commitment-uri: the entry points to where the commitment is served")
			}
		}
		in.WantA2A = in.CommitmentURI != ""
		in.WantAnnounce = in.MailboxKey != nil
		plan, err := buildPublishPlan(in)
		if err != nil {
			return err
		}
		rep := publishReport{NodeID: plan.NodeID, KeyID: plan.KeyID, Stream: plan.Stream, Digest: plan.Digest, Draft: len(to) == 0, Submitted: map[string]any{}}
		files := map[string][]byte{plan.NodeID + ".json": plan.Commitment, filepath.Join("policies", plan.NodeID+".json"): plan.Policy}
		for name, doc := range map[string]any{"ard-entry.json": plan.ARDEntry, "a2a-extension.json": plan.A2AEntry} {
			if m, _ := doc.(map[string]any); m != nil {
				if files[name], err = dumped(m); err != nil {
					return err
				}
			}
		}
		if plan.Announce != nil {
			if files["nostr-announce.json"], err = json.MarshalIndent(plan.Announce, "", "  "); err != nil {
				return err
			}
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			path := filepath.Join(out, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, files[name], 0o644); err != nil {
				return err
			}
			rep.Written = append(rep.Written, path)
		}
		var partial bool
		for _, carrier := range to {
			switch carrier {
			case "ard":
				if err := mergeARDFile(ardFile, plan.ARDEntry); err != nil {
					return err
				}
				rep.Submitted["ard"] = ardFile
			case "a2a":
				if err := mergeA2ACard(a2aFile, plan.A2AEntry); err != nil {
					return err
				}
				rep.Submitted["a2a"] = a2aFile
			case "nostr":
				var took []string
				var failed map[string]string
				if err := withPrivateNetwork(c, func() error {
					took, failed = publishAll(in.Relays, *plan.Announce)
					return nil
				}); err != nil {
					return err
				}
				rep.Submitted["nostr"] = map[string]any{"event_id": plan.Announce.ID, "took": took, "failed": failed}
				if len(took) == 0 {
					_ = Output(c, rep)
					return Hint(ErrInput, "no relay took the announce event")
				}
				partial = partial || len(failed) > 0
			}
		}
		if err := Output(c, rep); err != nil {
			return err
		}
		if partial {
			return ErrPartial
		}
		return nil
	}}
	f := cmd.Flags()
	f.String("staple", "", "The history card (history_card/1) to staple: the output of `deal standard-history staple --derivation history_card/1` (a file, or - for stdin); the card must be signed by this profile's signing key")
	f.String("out", "", "The directory the draft is written to: <node-id>.json (the commitment, exactly the octets every digest covers), policies/<node-id>.json, and the carrier entries")
	f.String("community", "", "The card's community: mesh-llm, muse or goose")
	f.String("node-id", "", "The node's id, 64 lowercase hex (default: the signing key)")
	f.String("endpoint", "", "ask.endpoint, how the node is asked (default: book:<the card's stream>)")
	f.StringSlice("binding", []string{"evidencebook/respond"}, "ask.bindings: the bindings the node answers on (repeat for each)")
	f.StringSlice("subject", []string{"checkpoints"}, "answers.subjects the node answers (repeat for each)")
	f.StringSlice("derivation", []string{"history_card/1"}, "answers.derivations the node answers (repeat for each)")
	f.StringSlice("predicate-class", []string{"performed:counterparty_half"}, "lcap.predicate_classes, <class>:<name> (repeat for each)")
	f.Bool("human-gated", true, "Whether a person approves each answer")
	f.String("answers-within", "PT24H", "How long an asker should wait, an ISO 8601 duration: not shorter than the node's poll interval")
	f.String("share-deal-half", "counterparty", "The signed disclosure policy's share_deal_half: counterparty, or empty to share none")
	f.String("checkpoint-key", "", "The key the card's checkpoints are signed by, 64 lowercase hex (default: read from the card; the profile's checkpoint.signing is checked against it when you give one)")
	f.String("mailbox-key", "", "A file holding the node's Nostr mailbox secret (`mailbox key new`, mode 600): it names ask.nostr.mailbox_key and signs the announce event")
	f.StringArray("nostr-relay", nil, "A relay (wss:) the node reads its mailbox on and announces to (repeat for each); with --mailbox-key it becomes ask.nostr")
	f.String("commitment-uri", "", "Where you serve the commitment octets (https:): the URI the carrier entries name; serve the file under --out as it is")
	f.String("ard-entry", "", "Your own listing entry for your ARD file (a JSON object with identifier urn:air:<publisher>:..., displayName, type, url): the verb adds trustManifest.identity (the publisher) and the commitment pointer, and leaves the rest as it is")
	f.StringSlice("to", nil, "Submit to these carriers (ard, a2a, nostr); needs --yes: ard merges the entry into --ard-file, a2a adds the extension to --a2a-card, nostr publishes the announce event to the --nostr-relay relays")
	f.Bool("yes", false, "The seller's yes to submit: nothing is merged or sent without it, and a draft needs none")
	f.String("ard-file", "", "With --to ard: the seller's own /.well-known/ard.json, created if absent")
	f.String("a2a-card", "", "With --to a2a: the seller's A2A Agent Card file")
	f.Bool("nostr-uri-only", false, "Leave the octets out of the announce event and name --commitment-uri alone, for a commitment too large for a relay")
	f.Bool("allow-private-network", false, "Reach loopback, private and link-local relays")
	f.StringArray("relay-map", nil, relayMapHelp)
	return cmd
}
