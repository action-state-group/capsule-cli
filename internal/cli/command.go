package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/cll"
	"github.com/spf13/cobra"
)

func selected(c *cobra.Command) (Profile, error) {
	name, _ := c.Flags().GetString("profile")
	p, err := loadProfile(name)
	if err != nil {
		return p, errors.Join(ErrInput, err)
	}
	return p, nil
}

var ErrInput = errors.New("invalid input or profile configuration")

func inputError(reason string) error { return errors.Join(ErrInput, errors.New(reason)) }

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return ErrInput
	}
	return nil
}
func output(c *cobra.Command, value any) error {
	// RawMessage preserves exact JSON numbers while promoting result fields.
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("command output must be a JSON object")
	}
	if _, exists := fields["spec_version"]; exists {
		return errors.New("command output uses reserved spec_version field")
	}
	fields["spec_version"] = json.RawMessage(`"capsule-cli-result/v1"`)
	return json.NewEncoder(c.OutOrStdout()).Encode(fields)
}

// SafeError intentionally never returns a driver/config/request error string:
// those may contain DSNs, SQL values, invalid secret flags, or service bodies.
func SafeError(err error) string {
	switch {
	case errors.Is(err, artifact.ErrUntrustedSigner):
		return "producer is not authorized by the profile trusted_keys"
	case errors.Is(err, ErrInput):
		return ErrInput.Error()
	case errors.Is(err, ErrConflict):
		return ErrConflict.Error()
	case errors.Is(err, ErrPending):
		return ErrPending.Error()
	case errors.Is(err, ErrReadOnlyCLL):
		return ErrReadOnlyCLL.Error()
	case errors.Is(err, ErrPartial):
		return ErrPartial.Error()
	default:
		return "operation failed; check input, selected profile, permissions and service availability (sensitive details suppressed)"
	}
}
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrInput):
		return 2
	case errors.Is(err, ErrPartial):
		return 3
	case errors.Is(err, ErrPending):
		return 4
	case errors.Is(err, ErrConflict):
		return 5
	default:
		return 1
	}
}

// NewCommand returns a fresh tree: no shared flag/config state across invocations.
func NewCommand() *cobra.Command {
	root := &cobra.Command{Use: "capsule", Short: "Seal, store and publish AAC Capsules using named profiles", Version: "0.1.0-dev", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("profile", "", "Required named target for every operation")
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error { return ErrInput })
	root.AddCommand(profileCommands())
	store := &cobra.Command{Use: "store"}
	init := &cobra.Command{Use: "init", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		t, e := openTarget(c.Context(), p, useInitialization)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		p.StoreID = t.storeID
		if e = saveProfile(p, true); e != nil {
			return e
		}
		return output(c, map[string]string{"store_id": t.storeID, "log_id": p.LogID, "status": "initialized"})
	}}
	store.AddCommand(init)
	root.AddCommand(store)
	sealCmd := &cobra.Command{Use: "seal", Short: "Seal to an explicit private artifact file; no database connection", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := selected(c)
		if e != nil {
			return e
		}
		request, _ := c.Flags().GetString("request")
		path, _ := c.Flags().GetString("output")
		if path == "" {
			return inputError("--output is required")
		}
		raw, e := readInput(request)
		if e != nil {
			return e
		}
		r, e := parseRequest(raw)
		if e != nil {
			return e
		}
		key, e := privateKey(p.Signing)
		if e != nil {
			return e
		}
		record, e := seal(r, key)
		if e != nil {
			return e
		}
		record, e = artifact.Prepare(record)
		if e != nil {
			return e
		}
		public, ok := key.Public().(ed25519.PublicKey)
		if !ok {
			return inputError("invalid signing key")
		}
		if _, e = artifact.Verify(record, []ed25519.PublicKey{public}); e != nil {
			return e
		}
		b, e := json.Marshal(record)
		if e != nil {
			return e
		}
		if e = atomicFile(path, b, false); e != nil {
			return e
		}
		return output(c, map[string]string{"capsule_id": record.CapsuleID, "artifact": path})
	}}
	sealCmd.Flags().String("request", "", "capsule-seal-request/v1 JSON file")
	sealCmd.Flags().String("output", "", "New artifact.Record JSON file, byte fields are base64")
	root.AddCommand(sealCmd)
	get := &cobra.Command{Use: "get", Short: "Read the artifact SDK record, not the CLL entry", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		id, _ := c.Flags().GetString("capsule-id")
		if b, e := hex.DecodeString(id); e != nil || len(b) != 32 {
			return inputError("--capsule-id is required")
		}
		t, e := openTarget(c.Context(), p, useArtifacts)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		r, e := t.artifacts.Get(c.Context(), id)
		if e != nil {
			return e
		}
		raw, _ := c.Flags().GetBool("raw")
		value := getRecordOutput(r, raw)
		path, _ := c.Flags().GetString("output")
		if path != "" {
			b, e := json.Marshal(value)
			if e != nil {
				return e
			}
			if e = atomicFile(path, b, false); e != nil {
				return e
			}
			return output(c, map[string]string{"capsule_id": id, "artifact": path})
		}
		return output(c, value)
	}}
	get.Flags().String("capsule-id", "", "Capsule ID")
	get.Flags().Bool("raw", false, "Preserve SDK byte fields as base64 for exact-byte export and verify")
	get.Flags().String("output", "", "Write readable JSON to a file; use --raw for a verifiable artifact.Record")
	root.AddCommand(get)
	verify := &cobra.Command{Use: "verify", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := selected(c)
		if e != nil {
			return e
		}
		path, _ := c.Flags().GetString("capsule")
		r, e := readRecord(path)
		if e != nil {
			return e
		}
		keys, e := parseKeys(p.TrustedKeys)
		if e != nil {
			return e
		}
		if len(keys) == 0 {
			if e = output(c, map[string]string{"producer_trust": "not_performed: no trusted key", "cll_inclusion": "not_performed", "business_truth": "not_performed"}); e != nil {
				return e
			}
			return ErrPartial
		}
		checks, e := artifact.Verify(r, keys)
		if e != nil {
			if outErr := output(c, map[string]string{"capsule_and_artifacts": "failed", "cll_inclusion": "not_performed"}); outErr != nil {
				return outErr
			}
			return e
		}
		missing := missingBindings(r)
		if e = output(c, map[string]any{"capsule_identity": "passed", "producer_signature_and_trust": "passed", "artifacts": checks, "missing_originals": missing, "cll_inclusion": "not_performed", "business_truth": "not_performed"}); e != nil {
			return e
		}
		if len(missing) > 0 {
			return ErrPartial
		}
		for _, check := range checks {
			if check.Bound && !check.Verified {
				return ErrPartial
			}
		}
		return nil
	}}
	verify.Flags().String("capsule", "", "artifact.Record JSON file")
	root.AddCommand(verify)
	publish := &cobra.Command{Use: "publish", Short: "Seal, persist artifacts, and append to CLL", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		if p.ReadOnly {
			return ErrReadOnlyCLL
		}
		path, _ := c.Flags().GetString("request")
		raw, e := readInput(path)
		if e != nil {
			return e
		}
		r, e := parseRequest(raw)
		if e != nil {
			return e
		}
		private, e := privateKey(p.Signing)
		if e != nil {
			return e
		}
		if e = requirePublisherKey(p, private); e != nil {
			return e
		}
		t, e := openTarget(c.Context(), p, usePublication)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		result, e := t.publish(c.Context(), r, private)
		if result.CapsuleID != "" {
			if outErr := output(c, result); outErr != nil {
				return outErr
			}
		}
		return e
	}}
	publish.Flags().String("request", "", "capsule-seal-request/v1 JSON file")
	root.AddCommand(publish)
	logs := &cobra.Command{Use: "cll"}
	root.AddCommand(logs)
	list := &cobra.Command{Use: "list", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		after, _ := c.Flags().GetUint64("after")
		through, _ := c.Flags().GetUint64("through")
		limit, _ := c.Flags().GetInt("limit")
		if limit < 1 || limit > cll.MaxScanLimit || after > cll.MaxPortableInteger || (through != 0 && through < after) {
			return inputError("invalid bounded range")
		}
		t, e := openTarget(c.Context(), p, useCLLRead)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		entries, e := t.log.ScanEntries(c.Context(), after, limit)
		if e != nil {
			return e
		}
		items := make([]map[string]any, 0, len(entries))
		next := after
		for _, entry := range entries {
			if through != 0 && entry.Seq > through {
				break
			}
			items = append(items, map[string]any{"sequence": entry.Seq, "capsule_id": hex.EncodeToString(entry.Value), "appended_at": entry.AppendedAt})
			next = entry.Seq
		}
		return output(c, map[string]any{"entries": items, "next_after": next, "log_id": p.LogID, "store_id": t.storeID})
	}}
	list.Flags().Uint64("after", 0, "Exclusive sequence lower bound")
	list.Flags().Uint64("through", 0, "Inclusive sequence upper bound (0 unbounded)")
	list.Flags().Int("limit", 100, "Page limit, at most 1000")
	logs.AddCommand(list)
	appendCmd := &cobra.Command{Use: "append", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		path, _ := c.Flags().GetString("capsule")
		r, e := readRecord(path)
		if e != nil {
			return e
		}
		keys, e := parseKeys(p.TrustedKeys)
		if e != nil {
			return e
		}
		if _, e = artifact.Verify(r, keys); e != nil {
			return e
		}
		if len(missingBindings(r)) != 0 {
			return ErrPartial
		}
		use := useCLL
		if p.Namespace != "" {
			use = usePublication
		}
		t, e := openTarget(c.Context(), p, use)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		if t.artifacts != nil {
			if e = t.artifacts.Put(c.Context(), r); e != nil {
				return e
			}
		}
		entry, e := appendRecord(c.Context(), t.log, r.CapsuleID)
		if e != nil {
			return e
		}
		return output(c, map[string]any{"capsule_id": r.CapsuleID, "sequence": entry.Seq, "log_id": p.LogID, "store_id": t.storeID})
	}}
	appendCmd.Flags().String("capsule", "", "Verified artifact.Record file; persisted first only when artifact storage is configured")
	logs.AddCommand(appendCmd)
	addCheckpointCommands(logs)
	root.SetHelpCommand(&cobra.Command{Use: "help [command]", RunE: func(c *cobra.Command, args []string) error {
		target, _, e := root.Find(args)
		if e != nil {
			return fmt.Errorf("unknown command")
		}
		return target.Help()
	}})
	return root
}
