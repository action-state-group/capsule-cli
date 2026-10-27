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

	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/spf13/cobra"
)

// keyCommands generates Ed25519 signing keys and derives their public key so a
// caller needs no external crypto tooling. The seed (secret) is written to a
// file in the exact hex format `--signing-key-file` reads; only the public key
// is printed. SEED_FILE is a positional argument.
func keyCommands() *cobra.Command {
	key := &cobra.Command{Use: "key", Short: "Generate and inspect Ed25519 signing keys"}
	generate := &cobra.Command{Use: "generate", Short: "Generate an Ed25519 signing key: write the seed file, print the public key", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("output")
		if path == "" {
			return InputError("--output is required: the path to write the new seed file to (it must not exist yet)")
		}
		public, private, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		// O_EXCL: never clobber an existing signing key, and guarantee the seed
		// is created owner-only (0600 applies only on create).
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if e != nil {
			return InputError("cannot create signing key file (it must not already exist)")
		}
		_, we := f.WriteString(hex.EncodeToString(private.Seed()))
		if e := errors.Join(we, f.Close()); e != nil {
			return InputError("could not write the seed to --output: check that its directory exists and is writable")
		}
		return Output(c, map[string]string{"public_key": hex.EncodeToString(public), "signing_key_file": path})
	}}
	generate.Flags().StringP("output", "o", "", "path to write the 32-byte seed (hex, mode 0600)")
	showPublic := &cobra.Command{Use: "show-public SEED_FILE", Short: "Print the public key for an existing Ed25519 seed file", Args: OneArg, RunE: func(c *cobra.Command, args []string) error {
		raw, e := ReadInput(args[0])
		if e != nil {
			return e
		}
		private, e := PrivateKey(Secret{Value: strings.TrimSpace(string(raw))})
		if e != nil {
			return e
		}
		pub, ok := private.Public().(ed25519.PublicKey)
		if !ok {
			return InputError("the seed file does not hold an Ed25519 seed (64 hex characters)")
		}
		return Output(c, map[string]string{"public_key": hex.EncodeToString(pub)})
	}}
	key.AddCommand(generate, showPublic)
	return key
}

func Selected(c *cobra.Command) (Profile, error) {
	name, _ := c.Flags().GetString("profile")
	if name == "" {
		// A plugin, and a command a plugin runs back in the core, inherit
		// the profile its caller selected.
		name = inheritedProfile()
	}
	if name == "" {
		return Profile{}, InputError(c.CommandPath() + " needs --profile NAME (see `capsulectl profile list`)")
	}
	p, err := LoadProfile(name)
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
		return InputError(fmt.Sprintf("%s has no flag %s; see --help", c.CommandPath(), flagUnknown.FindStringSubmatch(msg)[1]))
	case flagNoValue.MatchString(msg):
		return InputError(fmt.Sprintf("flag %s needs a value", flagNoValue.FindStringSubmatch(msg)[1]))
	case flagBadValue.MatchString(msg):
		return InputError(fmt.Sprintf("flag %s has a value of the wrong type; see --help for what it takes", flagBadValue.FindStringSubmatch(msg)[1]))
	default:
		return InputError(c.CommandPath() + ": a flag could not be parsed; see --help")
	}
}

// sealToFileCommand builds the seal/emit command shape: sign a request, prepare
// and self-verify the resulting artifact.Record, and write it to a file --
// never a database. `outputFlag` names the required output-path flag, so
// `seal` and `emit` can share one implementation while each keeping the flag
// name their own callers already expect.
func sealToFileCommand(use, outputFlag, short string) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		request, _ := c.Flags().GetString("request")
		path, _ := c.Flags().GetString(outputFlag)
		if path == "" {
			return InputError("--" + outputFlag + " is required")
		}
		raw, e := ReadInput(request)
		if e != nil {
			return e
		}
		r, e := ParseRequest(raw)
		if e != nil {
			return e
		}
		key, e := PrivateKey(p.Signing)
		if e != nil {
			return e
		}
		record, e := Seal(r, key)
		if e != nil {
			return e
		}
		record, e = artifact.Prepare(record)
		if e != nil {
			return e
		}
		public, ok := key.Public().(ed25519.PublicKey)
		if !ok {
			return InputError("the profile's signing key is not an Ed25519 key")
		}
		if _, e = artifact.Verify(record, []ed25519.PublicKey{public}); e != nil {
			return e
		}
		b, e := json.Marshal(record)
		if e != nil {
			return e
		}
		if e = AtomicFile(path, b, false); e != nil {
			return e
		}
		return Output(c, map[string]string{"capsule_id": record.CapsuleID, "artifact": path})
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
func InputError(reason string) error { return Hint(ErrInput, reason) }

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
func Hint(class error, message string) error { return &hintError{class: class, message: message} }

func NoArgs(c *cobra.Command, args []string) error {
	if len(args) != 0 {
		return InputError(fmt.Sprintf("%s takes no positional arguments (got %d); every input is a --flag, see --help", c.CommandPath(), len(args)))
	}
	return nil
}
func OneArg(c *cobra.Command, args []string) error {
	if len(args) != 1 {
		return InputError(fmt.Sprintf("%s takes exactly one positional argument (got %d): %s", c.CommandPath(), len(args), c.Use))
	}
	return nil
}
func Output(c *cobra.Command, value any) error {
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
	// A command that sets no voice is silent (capsule-cli#345), unless its
	// voice follows its outcome and is added after it ran.
	if _, exists := fields["voice"]; !exists && !byOutcome(c) {
		fields["voice"] = json.RawMessage(`"silent"`)
	}
	return json.NewEncoder(c.OutOrStdout()).Encode(fields)
}

// SafeError intentionally never returns a driver/config/request error string:
// those may contain DSNs, SQL values, invalid secret flags, or service bodies.
func SafeError(err error) string {
	var exit *PluginExit
	if errors.As(err, &exit) {
		// The plugin said it on stderr already.
		return ""
	}
	var fileErr *InputFileError
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
	case errors.Is(err, ErrHistoryNotAccepted):
		return ErrHistoryNotAccepted.Error()
	case errors.Is(err, ErrEmailUnverified):
		return ErrEmailUnverified.Error()
	case errors.Is(err, ErrObjectUnverified):
		return ErrObjectUnverified.Error()
	case errors.Is(err, ErrStatementInvalid):
		return ErrStatementInvalid.Error()
	case errors.Is(err, ErrPaused):
		return ErrPaused.Error()
	case errors.Is(err, ErrBreaking):
		// Like ErrSchemaInvalid: `contract diff` already printed the changes.
		return ErrBreaking.Error()
	case errors.Is(err, ErrNoAnswer):
		return ErrNoAnswer.Error()
	case errors.Is(err, ErrSchemaInvalid):
		// The detailed per-issue report was already printed by `contract
		// validate` itself; this is only the trailing summary line.
		return ErrSchemaInvalid.Error()
	default:
		return "operation failed; check input, selected profile, permissions and service availability (sensitive details suppressed)"
	}
}
func ExitCode(err error) int {
	var exit *PluginExit
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
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

// CLIVersion and CLICommit are reported by both --version and `doctor`, so the
// two can never silently drift apart. Release builds set them with
// -ldflags "-X github.com/action-state-group/capsule-cli/cli.CLIVersion=<tag>
// -X github.com/action-state-group/capsule-cli/cli.CLICommit=<sha>"
// (.github/workflows/release.yml); a plain `go build` reports the defaults.
var (
	CLIVersion = "0.1.0-dev"
	CLICommit  = "unknown"
)

// PluginVersion and PluginCommit are a first-party plugin's own release tag
// and commit, set with -X by the plugin's release build. In a plugin binary
// CLIVersion and CLICommit stay the capsule-cli release it embeds. Both are
// empty in capsulectl itself and in plugins built inside this repository.
var (
	PluginVersion string
	PluginCommit  string
)

// NewCommand returns a fresh tree: no shared flag/config state across
// invocations. It holds the core commands, the transitional aliases of the
// commands that moved into the capsulectl-book and capsulectl-deal plugins,
// and every plugin discovered on the trusted roots.
func NewCommand() *cobra.Command {
	return newCommand(true, nil)
}

// newCommand builds the tree. With discover, it adds the discovered plugins
// (and a placeholder for each first-party plugin that is missing); extra
// commands are added in process, each replacing a command of the same name.
func newCommand(discover bool, extra []*cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "capsulectl", Short: "Seal, store and publish AAC Capsules using named profiles", Version: CLIVersion, SilenceUsage: true, SilenceErrors: true}
	root.SetVersionTemplate("capsulectl {{.Version}} (commit " + CLICommit + ")\n")
	root.PersistentFlags().String("profile", "", "Named target for commands that operate on a profile")
	root.SetFlagErrorFunc(flagError)
	root.AddCommand(profileCommands())
	root.AddCommand(keyCommands())
	root.AddCommand(bundleCommands()...)
	root.AddCommand(countersignCommands())
	root.AddCommand(contractCommands())
	root.AddCommand(judgeCommands())
	root.AddCommand(calibrationCommands())
	root.AddCommand(canaryCommands())
	root.AddCommand(transitionalAliases()...)
	root.AddCommand(settlementCommands())
	root.AddCommand(presentationCommands())
	store := &cobra.Command{Use: "store", Short: "Initialize and verify the profile's artifact and CLL store"}
	init := &cobra.Command{Use: "init", Short: "Initialize the store and pin its store_id into the profile", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		if done, e := delegateLog(c, p, nil); done {
			return e
		}
		t, e := OpenTarget(c.Context(), p, UseInitialization)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		return Output(c, map[string]string{"log_id": p.LogID, "status": "initialized"})
	}}
	store.AddCommand(init)
	store.AddCommand(aliasCommand("migrate", aliasShort("book migrate"), func([]string) (string, []string, error) {
		return BookPluginName, []string{"migrate"}, nil
	}))
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
	get := &cobra.Command{Use: "get", Short: "Read the artifact SDK record, not the CLL entry", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		id, _ := c.Flags().GetString("capsule-id")
		if b, e := hex.DecodeString(id); e != nil || len(b) != 32 {
			return InputError("--capsule-id is required: a Capsule ID, 64 hex characters")
		}
		t, e := OpenTarget(c.Context(), p, UseArtifacts)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		r, e := t.Artifacts.Get(c.Context(), id)
		if errors.Is(e, artifact.ErrNotFound) && p.IsBook() {
			return Hint(ErrInput, "the artifact store holds no capsule "+id+"; a capsule published on a book profile has its record's id, not the one `seal` gives (publish reports both, as capsule_id and sealed_id, and `cll list` lists the record ids)")
		}
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
			if e = AtomicFile(path, b, false); e != nil {
				return e
			}
			return Output(c, map[string]string{"capsule_id": id, "artifact": path})
		}
		return Output(c, value)
	}}
	get.Flags().String("capsule-id", "", "Capsule ID")
	get.Flags().Bool("raw", false, "Preserve SDK byte fields as base64 for exact-byte export and verify; field openings (opening.* originals, which the capsule does not bind) are left out of either view")
	get.Flags().String("output", "", "Write readable JSON to a file; use --raw for a verifiable artifact.Record")
	root.AddCommand(get)
	verify := &cobra.Command{Use: "verify", Short: "Verify a Capsule's identity, signature, trust and bound artifacts, or check an Evidence Bundle or a History offline", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		holderKeys, _ := c.Flags().GetStringArray("holder-key")
		historyPath, _ := c.Flags().GetString("history")
		given := 0
		for _, name := range []string{"capsule", "bundle", "history"} {
			if v, _ := c.Flags().GetString(name); v != "" {
				given++
			}
		}
		switch {
		case given > 1:
			// An input error (exit 2), never a failed check.
			return InputError("give one of --capsule, --bundle and --history")
		case len(holderKeys) > 0 && historyPath == "":
			return InputError("--holder-key goes with --history")
		case historyPath != "" && c.Flags().Changed("witness-directory"):
			return InputError("--witness-directory goes with --bundle; a History carries no witness receipt to check")
		}
		if bundlePath, _ := c.Flags().GetString("bundle"); bundlePath != "" {
			return verifyBundleFile(c, bundlePath)
		}
		if historyPath != "" {
			return verifyHistoryFile(c, historyPath, holderKeys)
		}
		p, e := Selected(c)
		if e != nil {
			return e
		}
		path, _ := c.Flags().GetString("capsule")
		raw, e := ReadInput(path)
		if e != nil {
			return e
		}
		shape, e := capsuleFileShape(path, raw)
		if e != nil {
			return e
		}
		if shape == capsuleShapeBare {
			return verifyBareCapsule(c, p, path, raw)
		}
		var r artifact.Record
		if e = DecodeJSONAs("--capsule "+path, raw, &r); e != nil {
			return e
		}
		keys, e := ParseKeys(p.TrustedKeys)
		if e != nil {
			return e
		}
		if len(keys) == 0 {
			if e = Output(c, map[string]string{"shape": capsuleShapeRecord, "producer_trust": "not_performed: no trusted key", "cll_inclusion": "not_performed", "business_truth": "not_performed"}); e != nil {
				return e
			}
			return ErrPartial
		}
		checks, e := artifact.Verify(r, keys)
		if e != nil {
			if outErr := Output(c, map[string]string{"shape": capsuleShapeRecord, "capsule_and_artifacts": "failed", "cll_inclusion": "not_performed"}); outErr != nil {
				return outErr
			}
			return e
		}
		missing := MissingBindings(r)
		if e = Output(c, map[string]any{"shape": capsuleShapeRecord, "capsule_identity": "passed", "producer_signature_and_trust": "passed", "artifacts": checks, "missing_originals": missing, "cll_inclusion": "not_performed", "business_truth": "not_performed"}); e != nil {
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
	verify.Flags().String("capsule", "", "A capsule file: an artifact.Record (as `seal --output` and `get --raw` write) or a bare Agent Action Capsule with an inline signature and key_id (as capsule-emit seals); its shape is read from its members and named in the output")
	verify.Flags().String("bundle", "", "Evidence Bundle (evidence-bundle/v2) JSON file, verified offline from the file alone")
	verify.Flags().String("witness-directory", "", "With --bundle: a witness directory (witnesses.json format) naming the witnesses and keys whose receipts to check; without it no receipt is checked")
	verify.Flags().String("history", "", "A one-file History (personal_history_basic/1, the profile's §8.2 form): its envelopes and History properties are checked offline, from the file alone, with agent-action-capsule's evidence-interop/0 verifier")
	verify.Flags().StringArray("holder-key", nil, "With --history: a holder key you accept, as 64 lowercase hex characters, obtained from the holder by another channel (repeat for each); without one no History is accepted")
	root.AddCommand(NarrateByOutcome(verify, verifyNarration))
	publish := &cobra.Command{Use: "publish", Short: "Seal, persist artifacts, and append to CLL", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		if p.ReadOnly {
			return ErrReadOnlyCLL
		}
		if done, e := delegateLog(c, p, nil); done {
			return e
		}
		path, _ := c.Flags().GetString("request")
		raw, e := ReadInput(path)
		if e != nil {
			return e
		}
		r, e := ParseRequest(raw)
		if e != nil {
			return e
		}
		private, e := PrivateKey(p.Signing)
		if e != nil {
			return e
		}
		if e = RequirePublisherKey(p, private); e != nil {
			return e
		}
		t, e := OpenTarget(c.Context(), p, UsePublication)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		result, e := t.Publish(c.Context(), r, private)
		if result.CapsuleID != "" {
			if outErr := Output(c, result); outErr != nil {
				return outErr
			}
		}
		return e
	}}
	publish.Flags().String("request", "", "capsule-seal-request/v1 JSON file")
	root.AddCommand(publish)
	logs := &cobra.Command{Use: "cll", Short: "Inspect and append to the profile's checkpointed log"}
	root.AddCommand(logs)
	list := &cobra.Command{Use: "list", Short: "List CLL entries in sequence order", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		after, _ := c.Flags().GetUint64("after")
		through, _ := c.Flags().GetUint64("through")
		limit, _ := c.Flags().GetInt("limit")
		switch {
		case limit < 1 || limit > cll.MaxScanLimit:
			return InputError(fmt.Sprintf("--limit must be from 1 to %d", cll.MaxScanLimit))
		case after > cll.MaxPortableInteger:
			return InputError(fmt.Sprintf("--after must be at most %d", uint64(cll.MaxPortableInteger)))
		case through != 0 && through < after:
			return InputError("--through must be 0 (unbounded) or at least --after")
		}
		// --log-id reads another log of the same store: a deal profile keeps
		// one log per deal (deal/<deal id>) beside its cadence log.
		if logID, _ := c.Flags().GetString("log-id"); logID != "" {
			if !LogName.MatchString(logID) {
				return InputError("--log-id must be lowercase letters, digits and ._:/-, such as deal/deal-0123456789abcdef")
			}
			if p.HasPluginLog() {
				return InputError("--log-id does not apply to a profile whose log the capsulectl-" + p.LogOwner() + " plugin keeps: it has one log")
			}
			p.LogID = logID
		}
		if done, e := delegateLog(c, p, nil); done {
			return e
		}
		t, e := OpenTarget(c.Context(), p, UseCLLRead)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		if p.HasPluginLog() {
			all, _ := c.Flags().GetBool("all")
			items, next, e := backendFor(p).List(c.Context(), p, after, through, limit, all)
			if e != nil {
				return e
			}
			return Output(c, map[string]any{"entries": items, "next_after": next, "log_id": p.LogID})
		}
		entries, e := t.Log.ScanEntries(c.Context(), after, limit)
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
		return Output(c, map[string]any{"entries": items, "next_after": next, "log_id": p.LogID})
	}}
	list.Flags().Uint64("after", 0, "Exclusive sequence lower bound")
	list.Flags().Uint64("through", 0, "Inclusive sequence upper bound (0 unbounded)")
	list.Flags().Int("limit", 100, "Page limit, at most 1000")
	list.Flags().Bool("all", false, "Plugin-kept logs: also list the log's internal records")
	list.Flags().String("log-id", "", "Read this log of the profile's store instead of its log_id (a deal's log is deal/<deal id>)")
	logs.AddCommand(list)
	appendCmd := &cobra.Command{Use: "append", Short: "Append a Capsule ID to the CLL as a new entry", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		if done, e := delegateLog(c, p, nil); done {
			return e
		}
		path, _ := c.Flags().GetString("capsule")
		r, e := readRecord(path)
		if e != nil {
			return e
		}
		keys, e := ParseKeys(p.TrustedKeys)
		if e != nil {
			return e
		}
		if _, e = artifact.Verify(r, keys); e != nil {
			return e
		}
		if len(MissingBindings(r)) != 0 {
			return ErrPartial
		}
		use := UseCLL
		if p.Namespace != "" {
			use = UsePublication
		}
		t, e := OpenTarget(c.Context(), p, use)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		if t.Artifacts != nil {
			if e = t.Artifacts.Put(c.Context(), r); e != nil {
				return e
			}
		}
		if t.Book != nil {
			seq, warning, e := t.Book.Append(c.Context(), r)
			if e != nil {
				return e
			}
			result := map[string]any{"capsule_id": r.CapsuleID, "sequence": seq, "log_id": p.LogID}
			if warning != "" {
				result["warning"] = warning
			}
			return Output(c, result)
		}
		entry, e := appendRecord(c.Context(), t.Log, r.CapsuleID)
		if e != nil {
			return e
		}
		return Output(c, map[string]any{"capsule_id": r.CapsuleID, "sequence": entry.Seq, "log_id": p.LogID})
	}}
	appendCmd.Flags().String("capsule", "", "Verified artifact.Record file; persisted first only when artifact storage is configured")
	logs.AddCommand(appendCmd)
	addCheckpointCommands(logs)
	root.AddCommand(discoverCommand())
	root.AddCommand(mapCommand())
	root.AddCommand(NarrateByOutcome(doctorCommand(), doctorNarration))
	root.AddCommand(pluginGroup())
	if discover && !isPluginManagement(root, os.Args[1:]) {
		addPluginCommands(root)
	}
	replaceCommands(root, extra)
	applyGrafts(root)
	root.SetHelpCommand(&cobra.Command{Use: "help [command]", Short: "Help about any command", RunE: func(c *cobra.Command, args []string) error {
		target, _, e := root.Find(args)
		if e != nil {
			return fmt.Errorf("unknown command")
		}
		return target.Help()
	}})
	return root
}
