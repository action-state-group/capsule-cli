package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/cll"
	"github.com/spf13/cobra"
)

// keyCommands generates Ed25519 signing keys and derives their public key so a
// caller needs no external crypto tooling. The seed (secret) is written to a
// file in the exact hex format `--signing-key-file` reads; only the public key
// is printed. SEED_FILE is a positional argument.
func keyCommands() *cobra.Command {
	key := &cobra.Command{Use: "key", Short: "Generate and inspect Ed25519 signing keys"}
	generate := &cobra.Command{Use: "generate", Short: "Generate an Ed25519 signing key: write the seed file, print the public key", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("output")
		if path == "" {
			return inputError("--output is required: the path to write the new seed file to (it must not exist yet)")
		}
		public, private, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		// O_EXCL: never clobber an existing signing key, and guarantee the seed
		// is created owner-only (0600 applies only on create).
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if e != nil {
			return inputError("cannot create signing key file (it must not already exist)")
		}
		_, we := f.WriteString(hex.EncodeToString(private.Seed()))
		if e := errors.Join(we, f.Close()); e != nil {
			return inputError("could not write the seed to --output: check that its directory exists and is writable")
		}
		return output(c, map[string]string{"public_key": hex.EncodeToString(public), "signing_key_file": path})
	}}
	generate.Flags().StringP("output", "o", "", "path to write the 32-byte seed (hex, mode 0600)")
	showPublic := &cobra.Command{Use: "show-public SEED_FILE", Short: "Print the public key for an existing Ed25519 seed file", Args: oneArg, RunE: func(c *cobra.Command, args []string) error {
		raw, e := readInput(args[0])
		if e != nil {
			return e
		}
		private, e := privateKey(Secret{Value: strings.TrimSpace(string(raw))})
		if e != nil {
			return e
		}
		pub, ok := private.Public().(ed25519.PublicKey)
		if !ok {
			return inputError("the seed file does not hold an Ed25519 seed (64 hex characters)")
		}
		return output(c, map[string]string{"public_key": hex.EncodeToString(pub)})
	}}
	key.AddCommand(generate, showPublic)
	return key
}

func selected(c *cobra.Command) (Profile, error) {
	name, _ := c.Flags().GetString("profile")
	if name == "" {
		return Profile{}, inputError(c.CommandPath() + " needs --profile NAME (see `capsulectl profile list`)")
	}
	p, err := loadProfile(name)
	if err != nil {
		return p, errors.Join(ErrInput, err)
	}
	return p, nil
}

// flagUnknown and flagBadValue pick the flag name out of a flag-parsing error
// without its value: a mistyped value can be a secret typed into the wrong
// flag.
var (
	flagUnknown  = regexp.MustCompile(`^unknown (?:shorthand )?flag: (\S+)`)
	flagBadValue = regexp.MustCompile(`for "([^"]+)" flag`)
	flagNoValue  = regexp.MustCompile(`^flag needs an argument: (\S+)`)
)

// flagError names the flag a command line got wrong, never its value.
func flagError(c *cobra.Command, err error) error {
	msg := err.Error()
	switch {
	case flagUnknown.MatchString(msg):
		return inputError(fmt.Sprintf("%s has no flag %s; see --help", c.CommandPath(), flagUnknown.FindStringSubmatch(msg)[1]))
	case flagNoValue.MatchString(msg):
		return inputError(fmt.Sprintf("flag %s needs a value", flagNoValue.FindStringSubmatch(msg)[1]))
	case flagBadValue.MatchString(msg):
		return inputError(fmt.Sprintf("flag %s has a value of the wrong type; see --help for what it takes", flagBadValue.FindStringSubmatch(msg)[1]))
	default:
		return inputError(c.CommandPath() + ": a flag could not be parsed; see --help")
	}
}

// sealToFileCommand builds the seal/emit command shape: sign a request, prepare
// and self-verify the resulting artifact.Record, and write it to a file --
// never a database. `outputFlag` names the required output-path flag, so
// `seal` and `emit` can share one implementation while each keeping the flag
// name their own callers already expect.
func sealToFileCommand(use, outputFlag, short string) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := selected(c)
		if e != nil {
			return e
		}
		request, _ := c.Flags().GetString("request")
		path, _ := c.Flags().GetString(outputFlag)
		if path == "" {
			return inputError("--" + outputFlag + " is required")
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
			return inputError("the profile's signing key is not an Ed25519 key")
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
	cmd.Flags().String("request", "", "capsule-seal-request/v1 JSON file")
	cmd.Flags().String(outputFlag, "", "New artifact.Record JSON file, byte fields are base64")
	return cmd
}

var ErrInput = errors.New("invalid input or profile configuration")

// inputError is an input-class error (exit 2) whose reason is shown to the
// operator. The reason must name the field, flag or file at fault and what is
// expected, and must never carry a secret, a credential or a record's content
// (TestNoGenericInputErrors holds every call site to this).
func inputError(reason string) error { return hint(ErrInput, reason) }

// hintError is an error whose message tells the operator what to do next.
// It holds no secret -- no key, credential, DSN or record content -- so
// SafeError prints it after its class instead of the class alone.
type hintError struct {
	class   error
	message string
}

func (h *hintError) Error() string { return h.message }
func (h *hintError) Unwrap() error { return h.class }

// hint wraps a non-secret, actionable message under an error class (ErrInput
// or ErrConflict), which still decides the exit code.
func hint(class error, message string) error { return &hintError{class: class, message: message} }

func noArgs(c *cobra.Command, args []string) error {
	if len(args) != 0 {
		return inputError(fmt.Sprintf("%s takes no positional arguments (got %d); every input is a --flag, see --help", c.CommandPath(), len(args)))
	}
	return nil
}
func oneArg(c *cobra.Command, args []string) error {
	if len(args) != 1 {
		return inputError(fmt.Sprintf("%s takes exactly one positional argument (got %d): %s", c.CommandPath(), len(args), c.Use))
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
	var fileErr *inputFileError
	var schemaErr *schemaLoadError
	var hintErr *hintError
	switch {
	case errors.As(err, &hintErr):
		// Written to be shown: the class, then what to do about it.
		return hintErr.class.Error() + ": " + hintErr.message
	case errors.As(err, &fileErr):
		// The path is caller-supplied via a flag, so surfacing it discloses
		// nothing sensitive and distinguishes a missing input file from a
		// profile/configuration problem.
		return fileErr.Error()
	case errors.As(err, &schemaErr):
		// --schema is likewise caller-typed, not a profile secret.
		return schemaErr.Error()
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
	case errors.Is(err, ErrBundleInvalid):
		return ErrBundleInvalid.Error()
	case errors.Is(err, ErrEmailUnverified):
		return ErrEmailUnverified.Error()
	case errors.Is(err, ErrPaused):
		return ErrPaused.Error()
	case errors.Is(err, ErrBreaking):
		// Like ErrSchemaInvalid: `contract diff` already printed the changes.
		return ErrBreaking.Error()
	case errors.Is(err, ErrSchemaInvalid):
		// The detailed per-issue report was already printed by `contract
		// validate` itself; this is only the trailing summary line.
		return ErrSchemaInvalid.Error()
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
	case errors.Is(err, ErrAlarm):
		return 6
	case errors.Is(err, ErrPaused):
		return 7
	default:
		return 1
	}
}

// cliVersion and cliCommit are reported by both --version and `doctor`, so the
// two can never silently drift apart. Release builds set them with
// -ldflags "-X github.com/action-state-group/capsule-cli/internal/cli.cliVersion=<tag>
// -X github.com/action-state-group/capsule-cli/internal/cli.cliCommit=<sha>"
// (.github/workflows/release.yml); a plain `go build` reports the defaults.
var (
	cliVersion = "0.1.0-dev"
	cliCommit  = "unknown"
)

// NewCommand returns a fresh tree: no shared flag/config state across invocations.
func NewCommand() *cobra.Command {
	root := &cobra.Command{Use: "capsulectl", Short: "Seal, store and publish AAC Capsules using named profiles", Version: cliVersion, SilenceUsage: true, SilenceErrors: true}
	root.SetVersionTemplate("capsulectl {{.Version}} (commit " + cliCommit + ")\n")
	root.PersistentFlags().String("profile", "", "Named target for commands that operate on a profile")
	root.SetFlagErrorFunc(flagError)
	root.AddCommand(profileCommands())
	root.AddCommand(keyCommands())
	root.AddCommand(bundleCommands()...)
	root.AddCommand(countersignCommands())
	root.AddCommand(contractCommands())
	root.AddCommand(judgeCommands())
	root.AddCommand(calibrationCommands())
	root.AddCommand(dealCommands())
	root.AddCommand(releaseCommands())
	root.AddCommand(backfillCommands())
	root.AddCommand(canaryCommands())
	root.AddCommand(closeCommand(), reconcileCommand(), requestCommand(), respondCommand())
	store := &cobra.Command{Use: "store", Short: "Initialize and verify the profile's artifact and CLL store"}
	init := &cobra.Command{Use: "init", Short: "Initialize the store and pin its store_id into the profile", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		t, e := openTarget(c.Context(), p, useInitialization)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		return output(c, map[string]string{"log_id": p.LogID, "status": "initialized"})
	}}
	store.AddCommand(init)
	migrate := &cobra.Command{Use: "migrate", Short: "Move a jsonl profile's pre-book cll.jsonl into its evidence book, once, in order (signs)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := selected(c)
		if e != nil {
			return e
		}
		if p.Type != "jsonl" || p.ReadOnly {
			return inputError("store migrate applies to a writable jsonl profile")
		}
		newLogID, _ := c.Flags().GetString("log-id")
		result, e := migrateStore(c.Context(), p, newLogID, bookNow())
		if e != nil {
			return e
		}
		return output(c, result)
	}}
	migrate.Flags().String("log-id", "", "New log_id for the book; required when the retired log was ever checkpointed")
	store.AddCommand(migrate)
	root.AddCommand(store)
	root.AddCommand(sealToFileCommand("seal", "output", "Seal to an explicit private artifact file; no database connection"))
	// `emit` is the v4 verb name for the same operation `seal` already performs
	// (sign, prepare, self-verify, write an artifact.Record to disk -- no
	// database). It shares sealToFileCommand's implementation rather than being
	// rebuilt, and names its output flag `--seal-output` for parity with
	// `discover`'s flag of the same name. `seal` is left in place, unchanged:
	// it is already documented (README) and tested, and collapsing it into
	// `emit` would be a breaking rename this task did not ask for.
	root.AddCommand(sealToFileCommand("emit", "seal-output", "Emit a Capsule: seal to an explicit artifact file; no database connection"))
	get := &cobra.Command{Use: "get", Short: "Read the artifact SDK record, not the CLL entry", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		id, _ := c.Flags().GetString("capsule-id")
		if b, e := hex.DecodeString(id); e != nil || len(b) != 32 {
			return inputError("--capsule-id is required: a Capsule ID, 64 hex characters")
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
	verify := &cobra.Command{Use: "verify", Short: "Verify a Capsule's identity, signature, trust and bound artifacts, or an Evidence Bundle offline", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		if bundlePath, _ := c.Flags().GetString("bundle"); bundlePath != "" {
			return verifyBundleFile(c, bundlePath)
		}
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
	verify.Flags().String("bundle", "", "Evidence Bundle (evidence-bundle/v2) JSON file, verified offline from the file alone")
	verify.Flags().String("witness-directory", "", "With --bundle: a witness directory (witnesses.json format) naming the witnesses and keys whose receipts to check; without it no receipt is checked")
	verify.MarkFlagsMutuallyExclusive("capsule", "bundle")
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
	logs := &cobra.Command{Use: "cll", Short: "Inspect and append to the profile's checkpointed log"}
	root.AddCommand(logs)
	list := &cobra.Command{Use: "list", Short: "List CLL entries in sequence order", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		after, _ := c.Flags().GetUint64("after")
		through, _ := c.Flags().GetUint64("through")
		limit, _ := c.Flags().GetInt("limit")
		switch {
		case limit < 1 || limit > cll.MaxScanLimit:
			return inputError(fmt.Sprintf("--limit must be from 1 to %d", cll.MaxScanLimit))
		case after > cll.MaxPortableInteger:
			return inputError(fmt.Sprintf("--after must be at most %d", uint64(cll.MaxPortableInteger)))
		case through != 0 && through < after:
			return inputError("--through must be 0 (unbounded) or at least --after")
		}
		// --log-id reads another log of the same store: a deal profile keeps
		// one log per deal (deal/<deal id>) beside its cadence log.
		if logID, _ := c.Flags().GetString("log-id"); logID != "" {
			if !logName.MatchString(logID) {
				return inputError("--log-id must be lowercase letters, digits and ._:/-, such as deal/deal-0123456789abcdef")
			}
			if p.Type == "jsonl" {
				return inputError("--log-id does not apply to a jsonl profile: its one log is the evidence book")
			}
			p.LogID = logID
		}
		t, e := openTarget(c.Context(), p, useCLLRead)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		if p.Type == "jsonl" {
			all, _ := c.Flags().GetBool("all")
			items, next, e := listBookFiles(c.Context(), p, after, through, limit, all)
			if e != nil {
				return e
			}
			return output(c, map[string]any{"entries": items, "next_after": next, "log_id": p.LogID})
		}
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
		return output(c, map[string]any{"entries": items, "next_after": next, "log_id": p.LogID})
	}}
	list.Flags().Uint64("after", 0, "Exclusive sequence lower bound")
	list.Flags().Uint64("through", 0, "Inclusive sequence upper bound (0 unbounded)")
	list.Flags().Int("limit", 100, "Page limit, at most 1000")
	list.Flags().Bool("all", false, "jsonl profiles: also list the evidence book's internal records")
	list.Flags().String("log-id", "", "Read this log of the profile's store instead of its log_id (a deal's log is deal/<deal id>)")
	logs.AddCommand(list)
	appendCmd := &cobra.Command{Use: "append", Short: "Append a Capsule ID to the CLL as a new entry", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
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
		if t.book != nil {
			seq, warning, e := appendPublished(c.Context(), t.book.book, r)
			if e != nil {
				return e
			}
			result := map[string]any{"capsule_id": r.CapsuleID, "sequence": seq, "log_id": p.LogID}
			if warning != "" {
				result["warning"] = warning
			}
			return output(c, result)
		}
		entry, e := appendRecord(c.Context(), t.log, r.CapsuleID)
		if e != nil {
			return e
		}
		return output(c, map[string]any{"capsule_id": r.CapsuleID, "sequence": entry.Seq, "log_id": p.LogID})
	}}
	appendCmd.Flags().String("capsule", "", "Verified artifact.Record file; persisted first only when artifact storage is configured")
	logs.AddCommand(appendCmd)
	addCheckpointCommands(logs)
	root.AddCommand(discoverCommand())
	root.AddCommand(mapCommand())
	root.AddCommand(doctorCommand())
	root.AddCommand(resultCommands())
	addPluginCommands(root)
	root.SetHelpCommand(&cobra.Command{Use: "help [command]", Short: "Help about any command", RunE: func(c *cobra.Command, args []string) error {
		target, _, e := root.Find(args)
		if e != nil {
			return fmt.Errorf("unknown command")
		}
		return target.Help()
	}})
	return root
}
