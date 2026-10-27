// Package cli implements the profile-first Capsule command boundary.
//
// It is also the host package the first-party plugins (capsulectl-book and
// capsulectl-deal) build against: each plugin binary is this command tree
// plus its own commands (NewPluginCommand). Before v1 it carries no
// compatibility promise; a plugin pins an exact capsule-cli release
// (docs/design/326-plugins-leave-the-core.md, section 4.2).
package cli

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

var profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// The current CLL MySQL schema inherits database collation. Restrict v1 log
// names to lowercase ASCII without whitespace so distinct CLI targets cannot
// alias through case folding, accents or MySQL's trailing-space comparison.
var LogName = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,190}$`)

// Secret permits precisely one explicit source. Environment variables are only
// consulted when named here; they never override another profile setting.
type Secret struct {
	Value string `yaml:"value,omitempty" mapstructure:"value"`
	File  string `yaml:"file,omitempty" mapstructure:"file"`
	Env   string `yaml:"env,omitempty" mapstructure:"env"`
}

func (s Secret) validate() error {
	n := 0
	for _, v := range []string{s.Value, s.File, s.Env} {
		if v != "" {
			n++
		}
	}
	if n > 1 {
		return InputError("a secret may come from only one of a literal value, --*-file or --*-env; the profile sets more than one")
	}
	return nil
}
func emptySecretReason(s Secret) string {
	if s.File != "" {
		return "the secret file " + s.File + " is empty"
	}
	return "the environment variable " + s.Env + " named for a secret is unset or empty"
}

func (s Secret) resolve() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if s.File != "" {
		b, e := readProtected(s.File)
		if e != nil {
			return "", InputError("cannot read the secret file " + s.File + ": it must exist, be a regular file, and be readable only by its owner (chmod 600)")
		}
		s.Value = strings.TrimSpace(string(b))
	}
	if s.Env != "" {
		s.Value = os.Getenv(s.Env)
	}
	if (s.File != "" || s.Env != "") && s.Value == "" {
		return "", InputError(emptySecretReason(s))
	}
	return s.Value, nil
}
func (s Secret) redact() Secret {
	if s.Value != "" {
		s.Value = "[redacted]"
	}
	return s
}

type Profile struct {
	Name      string `yaml:"name" mapstructure:"name"`
	Type      string `yaml:"type" mapstructure:"type"`
	LogID     string `yaml:"log_id" mapstructure:"log_id"`
	Namespace string `yaml:"namespace" mapstructure:"namespace"`
	ReadOnly  bool   `yaml:"read_only,omitempty" mapstructure:"read_only"`
	// Log names who keeps the profile's log (capsule-cli#236, #428): LogCLL,
	// the core's own CLL, or the name of the plugin that keeps it, such as
	// LogBook, an evidence book kept by the capsulectl-book plugin. Only a
	// jsonl profile with a log_id can name a plugin, and a profile keeps its
	// owner for good. A jsonl profile with a log_id saved before the field
	// existed reads as a book.
	Log string `yaml:"log,omitempty" mapstructure:"log" json:",omitempty"`
	// Operator is the AAC operator value the book verbs write into every
	// record they seal. Only those verbs require it.
	Operator string `yaml:"operator,omitempty" mapstructure:"operator"`
	// ClockTolerance (a Go duration, default 5m, at most 1h) is how far the
	// book's last commit time may sit ahead of this machine's clock before
	// the book verbs refuse to write. Within it, a record written while the
	// clock is behind the book is committed at the book's last time, so a
	// record appended up to that long before a period ends may be counted in
	// the next period.
	ClockTolerance string `yaml:"clock_tolerance,omitempty" mapstructure:"clock_tolerance"`
	Connection     struct {
		Host     string `yaml:"host" mapstructure:"host"`
		Port     int    `yaml:"port" mapstructure:"port"`
		Database string `yaml:"database" mapstructure:"database"`
		TLS      string `yaml:"tls" mapstructure:"tls"`
		// URL is the base address of a remote profile type's transport; the
		// storage-backed types leave it empty.
		URL string `yaml:"url,omitempty" mapstructure:"url"`
		// RequesterID is the requester id a remote profile's evidence
		// requests name: the node's own full peer id, set by the operator
		// (never derived), so the party digests the exact request sent.
		RequesterID string `yaml:"requester_id,omitempty" mapstructure:"requester_id"`
	} `yaml:"connection" mapstructure:"connection"`
	Credentials struct {
		Username string `yaml:"username" mapstructure:"username"`
		Password Secret `yaml:"password,omitempty" mapstructure:"password"`
		// Token is an optional bearer token for a remote profile type's URL.
		Token Secret `yaml:"token,omitempty" mapstructure:"token"`
	} `yaml:"credentials" mapstructure:"credentials"`
	Signing     Secret   `yaml:"signing,omitempty" mapstructure:"signing"`
	TrustedKeys []string `yaml:"trusted_keys,omitempty" mapstructure:"trusted_keys"`
	Checkpoint  struct {
		Signing     Secret   `yaml:"signing,omitempty" mapstructure:"signing"`
		TrustedKeys []string `yaml:"trusted_keys,omitempty" mapstructure:"trusted_keys"`
		Endpoint    string   `yaml:"endpoint,omitempty" mapstructure:"endpoint"`
		PublicKey   string   `yaml:"public_key,omitempty" mapstructure:"public_key"`
		Token       Secret   `yaml:"token,omitempty" mapstructure:"token"`
	} `yaml:"checkpoint,omitempty" mapstructure:"checkpoint"`
	// Cadence is when a deal profile publishes to its witness, its checkpoint
	// cadence: on time alone (Interval, default 5m, each tick moved by a
	// random amount within +/-Jitter, default 1m), never on activity. PadBucket (default 1) pads
	// each tick's entries to a multiple of it with padding records.
	Cadence struct {
		Interval  string `yaml:"interval,omitempty" mapstructure:"interval"`
		Jitter    string `yaml:"jitter,omitempty" mapstructure:"jitter"`
		PadBucket uint64 `yaml:"pad_bucket,omitempty" mapstructure:"pad_bucket"`
	} `yaml:"cadence,omitempty" mapstructure:"cadence"`
	// Materiality names the materiality predicate (a materiality-predicate/v0
	// document) a deal check evaluates to decide which of the agent's own
	// picks pause, pinned by its digest (SHA-256 of its JCS bytes): a check
	// refuses a predicate that no longer matches. None configured: every
	// pick pauses. Setting it is the user's (deal init or profile update
	// --materiality): it is policy, never the agent's to change.
	Materiality struct {
		Predicate string `yaml:"predicate,omitempty" mapstructure:"predicate"`
		Digest    string `yaml:"digest,omitempty" mapstructure:"digest"`
	} `yaml:"materiality,omitempty" mapstructure:"materiality"`
	// RulesChecker is a deal profile's external rules checker, run at every
	// deal check (rules_checker.go): the user's policy (profile update
	// --rules-checker), pinned by its executable's SHA-256.
	RulesChecker ProfileRulesChecker `yaml:"rules_checker,omitempty" mapstructure:"rules_checker"`
	// Wording is the wording pack this profile speaks with (narration.go):
	// a complete aac.wording-pack/v0, pinned by its SHA-256 (profile update
	// --wording). None: the built-in neutral pack.
	Wording ProfileWording `yaml:"wording,omitempty" mapstructure:"wording" json:",omitzero"`
}

// ProfileRulesChecker is a pinned external rules checker.
type ProfileRulesChecker struct {
	Command          []string `yaml:"command,omitempty" mapstructure:"command"`
	SHA256           string   `yaml:"sha256,omitempty" mapstructure:"sha256"`
	Timeout          string   `yaml:"timeout,omitempty" mapstructure:"timeout"`
	DefinitionDigest string   `yaml:"definition_digest,omitempty" mapstructure:"definition_digest"`
	// DefinitionDigests holds a pinned set of definition digests, and
	// DefinitionDigest is then empty. profile update --rules-checker stores
	// a set of two or more here and one digest in DefinitionDigest alone, as
	// before the set.
	DefinitionDigests []string `yaml:"definition_digests,omitempty" mapstructure:"definition_digests" json:",omitempty"`
}

// pinnedDigests is the set of definition digests a checker's ruleset must
// report one of; none pins no ruleset.
func (r ProfileRulesChecker) PinnedDigests() []string {
	if len(r.DefinitionDigests) > 0 {
		return r.DefinitionDigests
	}
	if r.DefinitionDigest != "" {
		return []string{r.DefinitionDigest}
	}
	return nil
}

// The two owners a profile's log can have (Profile.Log).
const (
	LogCLL  = "cll"
	LogBook = "book"
)

// LogOwner is the plugin that keeps the profile's log, or "" when the core
// does (capsule-cli#428): a jsonl profile with a log_id whose log is not
// declared a plain CLL. One saved before the log field existed has no declared
// owner and reads as LogBook until that default goes.
func (p Profile) LogOwner() string {
	switch {
	case p.Log == LogCLL:
		return ""
	case p.Type == "jsonl" && p.LogID != "":
		return FirstNonEmpty(p.Log, LogBook)
	case p.Type == "sqlite" && p.Namespace != "":
		// A store the plugin writes and the core only reads. A deal profile
		// (namespace deal) saved before the field existed reads as the deal
		// plugin's until that default goes.
		if p.Log == "" && p.IsShelf() {
			return DealPluginName
		}
		return p.Log
	}
	return ""
}

// HasPluginLog reports whether a plugin keeps the profile's log in a store of
// its own (a jsonl profile): the core needs the plugin's backend to open it.
// A sqlite store a plugin owns is a plain CLL the core reads (HasOwnedStore).
func (p Profile) HasPluginLog() bool { return p.Type == "jsonl" && p.LogOwner() != "" }

// HasOwnedStore reports whether a plugin owns the profile's sqlite store: the
// core reads it and never writes it.
func (p Profile) HasOwnedStore() bool { return p.Type == "sqlite" && p.LogOwner() != "" }

// IsBook reports whether the book plugin keeps the profile's log: an evidence
// book. Every record a book seals carries the profile's operator.
func (p Profile) IsBook() bool { return p.HasPluginLog() && p.LogOwner() == LogBook }

// validLogOwner reports whether name can own a profile's log: LogCLL, or a
// plugin name that is not a core command.
func validLogOwner(name string) bool {
	return name == LogCLL || (pluginNameGrammar.MatchString(name) && !coreCommandNames()[name])
}

// IsShelf reports whether the profile is a deal profile (`deal init`): its
// logs, each deal's and the cadence log, are written by capsulectl deal
// alone, under the deal store's writer lock (capsule-cli#242).
func (p Profile) IsShelf() bool { return p.Type == "sqlite" && p.Namespace == "deal" }

// invisibleRune reports whitespace and format characters (Unicode category
// Cf: U+200B, U+FEFF, U+200D and the like), which print as nothing.
func invisibleRune(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) }

// normalizeOperator is the operator as profile create and update store it,
// whether it came from --operator or --interactive: without leading or
// trailing whitespace or format characters.
func normalizeOperator(s string) string { return strings.TrimFunc(s, invisibleRune) }

// operatorVisible reports whether s has at least one character that prints:
// a graphic rune that is neither whitespace nor a format character.
func operatorVisible(s string) bool {
	for _, r := range s {
		if unicode.IsGraphic(r) && !invisibleRune(r) {
			return true
		}
	}
	return false
}

// operatorMissing reports a book whose operator has no visible character:
// empty, whitespace, or only invisible characters such as U+200B. Such a
// profile is refused when it is created or updated, and refused at seal time
// if an earlier capsulectl saved it.
func (p Profile) OperatorMissing() bool {
	return p.IsBook() && !operatorVisible(p.Operator)
}

func (p Profile) validate() error {
	if _, err := p.ClockToleranceDuration(); err != nil {
		return err
	}
	if !profileName.MatchString(p.Name) || (p.Type != "mysql" && p.Type != "sqlite" && p.Type != "jsonl" && p.Type != RemoteProfileType) {
		return InputError("profile needs a valid name and a mysql, sqlite, jsonl, or " + RemoteProfileType + " type")
	}
	if (p.LogID != "" && !LogName.MatchString(p.LogID)) || (p.Namespace != "" && !profileName.MatchString(p.Namespace)) {
		return InputError("log_id must be lowercase letters, digits and ._:/- (starting with a letter or digit, at most 191 characters); namespace must be letters, digits, _ and - (at most 64)")
	}
	if p.LogID == "" && p.Namespace == "" {
		return InputError("configure an artifact namespace, a log_id, or both")
	}
	switch {
	case p.Log != "" && !validLogOwner(p.Log):
		return InputError("log must be cll (the core keeps the log) or the name of the plugin that keeps it (capsulectl-<name>); a core command name is not a plugin")
	case p.Log != "" && p.Log != LogCLL && !((p.Type == "jsonl" && p.LogID != "") || (p.Type == "sqlite" && p.Namespace != "")):
		return InputError("only a jsonl profile with a log_id or a sqlite profile with a namespace can name a plugin as its log owner (log: " + p.Log + ")")
	}
	if p.Type == RemoteProfileType {
		if e := validateRemoteConnection(p); e != nil {
			return e
		}
	} else if p.Type == "sqlite" || p.Type == "jsonl" {
		// SQLite and JSONL are local backends: connection.database is a filesystem
		// path (a file for sqlite; a directory holding artifacts.jsonl and
		// cll.jsonl for jsonl) and no host/port/TLS applies.
		if p.Connection.Database == "" {
			return InputError("sqlite/jsonl profile needs a filesystem path in connection.database")
		}
	} else {
		if p.Connection.Host == "" || p.Connection.Database == "" || p.Connection.Port < 1 || p.Connection.Port > 65535 {
			return InputError("profile needs a MySQL host, port and database")
		}
		if p.Connection.TLS != "true" && p.Connection.TLS != "false" {
			return InputError("mysql TLS must be true or false; insecure fallback is unsupported")
		}
	}
	for _, s := range []Secret{p.Credentials.Password, p.Credentials.Token, p.Signing, p.Checkpoint.Signing, p.Checkpoint.Token} {
		if e := s.validate(); e != nil {
			return e
		}
	}
	if _, e := ParseKeys(p.TrustedKeys); e != nil {
		return e
	}
	if _, e := ParseKeys(p.Checkpoint.TrustedKeys); e != nil {
		return e
	}
	if len(p.RulesChecker.DefinitionDigests) > 0 && p.RulesChecker.DefinitionDigest != "" {
		return InputError("rules_checker holds definition_digest or definition_digests, not both: edit the profile file to keep one (profile update reads the file first)")
	}
	for _, d := range p.RulesChecker.DefinitionDigests {
		if !Hex64.MatchString(d) {
			return InputError("rules_checker definition_digests must each be 64 lowercase hex characters: edit the profile file to correct them")
		}
	}
	return nil
}
func ParseKeys(values []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(values))
	for _, v := range values {
		b, e := hex.DecodeString(v)
		if e != nil || len(b) != 32 {
			return nil, InputError("trusted keys must be 32-byte Ed25519 public keys in hex")
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, nil
}
func PrivateKey(s Secret) (ed25519.PrivateKey, error) {
	v, e := s.resolve()
	if e != nil {
		return nil, e
	}
	b, e := hex.DecodeString(v)
	if e != nil || len(b) != ed25519.SeedSize {
		return nil, InputError("signing secret must contain a 32-byte Ed25519 seed in hex")
	}
	return ed25519.NewKeyFromSeed(b), nil
}
func readProtected(path string) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, InputError("file must be regular and accessible only to its owner")
	}
	return os.ReadFile(path)
}
func ProfilePath(name string) (string, error) {
	if !profileName.MatchString(name) {
		return "", InputError("a profile name is required: letters, digits, _ and - (starting with a letter or digit, at most 64 characters)")
	}
	dir, e := ProfilesDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(dir, name+".yaml"), nil
}

func ProfilesDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		base = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(base) {
		return "", InputError("XDG_CONFIG_HOME must be an absolute path when it is set")
	}
	return filepath.Join(base, "capsule", "profiles"), nil
}
func LoadProfile(name string) (Profile, error) {
	path, e := ProfilePath(name)
	if e != nil {
		return Profile{}, e
	}
	raw, e := readProtected(path)
	if e != nil {
		return Profile{}, InputError("profile " + name + " was not found, or its file is readable by others than its owner (chmod 600): see `capsulectl profile list`, or create it with `capsulectl profile create`")
	}
	var p Profile
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if e = d.Decode(&p); e != nil {
		return Profile{}, InputError("profile " + name + " has a field capsulectl does not know, or a field of the wrong type: compare it with `capsulectl profile show`")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, InputError("the profile file must hold exactly one YAML document")
	}
	if p.Name != name {
		return p, InputError("the profile file's name field must match its filename (NAME.yaml holds name: NAME)")
	}
	return p, p.validate()
}

// atomicFile uses link for create-only admission and rename for explicit updates.
// fsync on both the file and directory makes profile changes restart-durable.
func AtomicFile(path string, data []byte, replace bool) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".capsule-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if replace {
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return InputError("update requires an existing owner-protected regular file")
		}
		err = os.Rename(name, path)
	} else {
		err = os.Link(name, path)
	}
	if err != nil {
		return err
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	return errors.Join(d.Sync(), d.Close())
}
func SaveProfile(p Profile, replace bool) error {
	if e := p.validate(); e != nil {
		return e
	}
	b, e := yaml.Marshal(p)
	if e != nil {
		return e
	}
	path, e := ProfilePath(p.Name)
	if e != nil {
		return e
	}
	return AtomicFile(path, b, replace)
}
func profileCommands() *cobra.Command {
	group := &cobra.Command{Use: "profile", Short: "Create, inspect or explicitly update a named target"}
	list := &cobra.Command{Use: "list", Short: "List configured profile names", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		dir, e := ProfilesDir()
		if e != nil {
			return e
		}
		entries, e := os.ReadDir(dir)
		if errors.Is(e, os.ErrNotExist) {
			return Output(c, map[string]any{"profiles": []string{}})
		}
		if e != nil {
			return e
		}
		profiles := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".yaml" {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".yaml")
			if profileName.MatchString(name) {
				profiles = append(profiles, name)
			}
		}
		slices.Sort(profiles)
		return Output(c, map[string]any{"profiles": profiles})
	}}
	show := &cobra.Command{Use: "show NAME", Short: "Show a profile's identity, backend and key-file paths (secrets redacted)", Args: OneArg, RunE: func(c *cobra.Command, args []string) error {
		p, e := LoadProfile(args[0])
		if e != nil {
			return errors.Join(ErrInput, e)
		}
		p.Credentials.Password = p.Credentials.Password.redact()
		p.Credentials.Token = p.Credentials.Token.redact()
		p.Signing = p.Signing.redact()
		p.Checkpoint.Signing = p.Checkpoint.Signing.redact()
		p.Checkpoint.Token = p.Checkpoint.Token.redact()
		return Output(c, p)
	}}
	group.AddCommand(list, show, profileRetireCommand())
	for _, update := range []bool{false, true} {
		verb := "create"
		if update {
			verb = "update"
		}
		c := &cobra.Command{Use: verb, Args: NoArgs}
		f := c.Flags()
		f.String("name", "", "New profile name")
		f.Bool("interactive", false, "Ask for missing nonsecret connection fields")
		fields := map[string]string{"type": "type", "log": "log", "log-id": "log_id", "namespace": "namespace", "mysql-host": "connection.host", "mysql-database": "connection.database", "mysql-tls": "connection.tls", "mysql-user": "credentials.username", "checkpoint-endpoint": "checkpoint.endpoint", "checkpoint-public-key": "checkpoint.public_key", "operator": "operator", "clock-tolerance": "clock_tolerance", "url": "connection.url", "requester-id": "connection.requester_id"}
		for flag := range fields {
			f.String(flag, "", "Profile setting")
		}
		f.Lookup("log").Usage = "Who keeps the profile's log: cll (the core's own CLL) or the name of the plugin that keeps it (capsulectl-<name>; jsonl profiles with a log_id only). A jsonl profile with a log_id and no log reads as book until that default is removed. Fixed once set"
		f.Int("mysql-port", 3306, "MySQL port")
		f.String("sqlite-path", "", "SQLite database file path (sets connection.database for --type sqlite)")
		f.String("jsonl-path", "", "JSONL storage directory (sets connection.database for --type jsonl)")
		f.Bool("read-only", false, "Reject operations requiring writes")
		f.StringSlice("trusted-key", nil, "Trusted producer public key hex (repeatable)")
		f.StringSlice("checkpoint-trusted-key", nil, "Trusted checkpoint signer public key hex (repeatable)")
		f.String("materiality", "", "A deal profile's materiality predicate (a materiality-predicate/v0 JSON file, or builtin:neutral), pinned by its digest; empty to remove it (every agent pick then pauses). Policy: the user's to set, never the agent's")
		f.String("wording", "", "The wording pack this profile speaks with: a complete aac.wording-pack/v0 JSON file holding every key of the built-in pack, pinned by its SHA-256; empty to go back to the built-in neutral pack")
		f.String("rules-checker", "", "A deal profile's external rules checker: a JSON file {\"command\": [\"/absolute/path\", \"arg\", ...], \"timeout\": \"10s\", \"definition_digest\": \"...\"} (or \"definition_digests\": [\"...\", ...], a set of rulesets any of which it may report), its executable under a trusted plugin root and pinned by its SHA-256; empty to remove it. Policy: the user's to set, never the agent's")
		secrets := map[string]string{"mysql-password": "credentials.password", "signing-key": "signing", "checkpoint-signing-key": "checkpoint.signing", "checkpoint-token": "checkpoint.token", "token": "credentials.token"}
		for flag := range secrets {
			f.String(flag, "", "Literal secret; prefer file/env reference")
			f.String(flag+"-file", "", "Owner-protected secret file")
			f.String(flag+"-env", "", "Explicit secret environment-variable name")
		}
		c.RunE = func(c *cobra.Command, _ []string) error {
			v := viper.New()
			v.SetConfigType("yaml")
			var before Profile
			if update {
				p, e := Selected(c)
				if e != nil {
					return e
				}
				before = p
				b, e := yaml.Marshal(p)
				if e != nil {
					return e
				}
				if e = v.ReadConfig(bytes.NewReader(b)); e != nil {
					return e
				}
			} else {
				name, _ := c.Flags().GetString("name")
				v.SetDefault("name", name)
			}
			v.SetDefault("type", "mysql")
			v.SetDefault("namespace", "capsule")
			v.SetDefault("connection.tls", "true")
			v.SetDefault("connection.port", 3306)
			binds := map[string]string{}
			for flag, key := range fields {
				binds[flag] = key
			}
			for flag, key := range map[string]string{"mysql-port": "connection.port", "read-only": "read_only", "trusted-key": "trusted_keys", "checkpoint-trusted-key": "checkpoint.trusted_keys"} {
				binds[flag] = key
			}
			for flag, key := range secrets {
				binds[flag] = key + ".value"
				binds[flag+"-file"] = key + ".file"
				binds[flag+"-env"] = key + ".env"
			}
			for flag, key := range binds {
				if e := v.BindPFlag(key, c.Flags().Lookup(flag)); e != nil {
					return e
				}
			}
			var p Profile
			if e := v.UnmarshalExact(&p); e != nil {
				return InputError("the profile settings could not be applied: a value has the wrong type for its field")
			}
			if sp, _ := c.Flags().GetString("sqlite-path"); sp != "" {
				p.Connection.Database = sp
			}
			if jp, _ := c.Flags().GetString("jsonl-path"); jp != "" {
				p.Connection.Database = jp
			}
			interactive, _ := c.Flags().GetBool("interactive")
			if interactive {
				reader := bufio.NewReader(c.InOrStdin())
				// Prompt only for fields the selected backend needs; SQLite and
				// MySQL are peers, so neither backend's connection fields are asked
				// for the other.
				type prompt struct {
					label  string
					target *string
				}
				prompts := []prompt{{"Name", &p.Name}}
				switch p.Type {
				case "sqlite":
					prompts = append(prompts, prompt{"SQLite database file path", &p.Connection.Database})
				case "jsonl":
					prompts = append(prompts, prompt{"JSONL storage directory", &p.Connection.Database})
				case RemoteProfileType:
					prompts = append(prompts, prompt{"Base URL", &p.Connection.URL}, prompt{"The node's own full peer id (64 hex)", &p.Connection.RequesterID})
				default:
					prompts = append(prompts, prompt{"MySQL host", &p.Connection.Host}, prompt{"Database", &p.Connection.Database})
				}
				prompts = append(prompts, prompt{"Log ID (optional for artifact-only)", &p.LogID})
				if p.Type == "mysql" {
					prompts = append(prompts, prompt{"MySQL user", &p.Credentials.Username})
				}
				ask := func(item prompt) error {
					if *item.target != "" {
						return nil
					}
					if _, e := fmt.Fprint(c.ErrOrStderr(), item.label+": "); e != nil {
						return e
					}
					s, e := reader.ReadString('\n')
					if e != nil {
						return InputError("guided configuration requires input; use flags for automation")
					}
					*item.target = strings.TrimSpace(s)
					return nil
				}
				for _, item := range prompts {
					if e := ask(item); e != nil {
						return e
					}
				}
				// Only a book seals records with the operator, and whether this
				// profile is one depends on the log ID just asked for.
				if p.IsBook() {
					if e := ask(prompt{"Operator (who signs the book's records)", &p.Operator}); e != nil {
						return e
					}
				}
			}
			// A witness endpoint and its public key travel together: never write a
			// profile whose every witnessed milestone is bound to fail.
			if update && c.Flags().Changed("checkpoint-endpoint") && !c.Flags().Changed("checkpoint-public-key") && p.Checkpoint.Endpoint != "" {
				return Hint(ErrInput, "--checkpoint-endpoint needs --checkpoint-public-key <64-hex Ed25519 key> in the same call: the key identifies that witness")
			}
			if e := checkWitnessConfig(p); e != nil {
				return e
			}
			if c.Flags().Changed("materiality") {
				path, _ := c.Flags().GetString("materiality")
				if e := PinMateriality(&p, path); e != nil {
					return e
				}
			}
			if c.Flags().Changed("rules-checker") {
				path, _ := c.Flags().GetString("rules-checker")
				if e := pinRulesChecker(&p, path); e != nil {
					return e
				}
			}
			if c.Flags().Changed("wording") {
				path, _ := c.Flags().GetString("wording")
				if e := pinWording(&p, path); e != nil {
					return e
				}
			}
			// The log's owner is declared when the profile is made, and a
			// jsonl profile with a log_id is a book unless it says cll. It
			// never changes: the other owner would not see the log.
			if p.Type == "jsonl" && p.LogID != "" && p.Log == "" {
				p.Log = LogBook
			}
			if update && before.LogID != "" && before.LogOwner() != p.LogOwner() {
				return InputError("a profile's log stays with the owner it was made with (log: " + FirstNonEmpty(before.LogOwner(), LogCLL) + "); make a new profile for the other")
			}
			p.Operator = normalizeOperator(p.Operator)
			if p.OperatorMissing() {
				return InputError("--operator is required for a jsonl profile with a log_id: the profile is an evidence book, and the operator names who signs every record it seals")
			}
			if e := SaveProfile(p, update); e != nil {
				return e
			}
			return Output(c, map[string]string{"profile": p.Name, "status": "saved"})
		}
		group.AddCommand(c)
	}
	return group
}
