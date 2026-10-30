package cli

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A deal profile may pin an external rules checker: a command the user
// chose, run at every deal check on the record of what is about to happen.
// It reports a neutral result (external-check-result/v0: the ruleset it
// evaluated, by id and definition digest, a verdict and its findings), and
// the check seals those values as data. capsulectl knows no ruleset of its
// own: an id appears in a check only because the pinned checker reported it.
//
// Pinning is the user's policy (profile update --rules-checker FILE): the
// command's executable must live under a trusted plugin root and is pinned
// by its SHA-256, and the ruleset's definition digest may be pinned too. A
// checker that was swapped, fails, times out or reports anything else pauses
// the check, saying why; a profile with no checker says so and does not.

const (
	ExternalCheckSchema = "external-check-result/v0"
	rulesTimeoutDefault = 10 * time.Second
	rulesTimeoutMax     = 60 * time.Second
	RulesMaxOutput      = 1 << 20
	RulesMaxExecutable  = 256 << 20
	ExternalCheckInput  = "external-check-input/v0"
	// The history a checker is given: the profile's sealed acts with an
	// amount, from every deal, sealed in the last rulesHistoryDays days, the
	// most recent rulesHistoryMax of them.
	RulesHistoryDays = 31
)

// rulesCheckerPin is the file `profile update --rules-checker` reads.
type rulesCheckerPin struct {
	Command          []string `json:"command"`
	Timeout          string   `json:"timeout,omitempty"`
	DefinitionDigest string   `json:"definition_digest,omitempty"`
	// DefinitionDigests pins a set of rulesets: the checker may report any
	// of them. Together with DefinitionDigest, the set is both.
	DefinitionDigests []string `json:"definition_digests,omitempty"`
	// Roles pins a pack and its definition digests per deal role (buyer,
	// seller); the flat digests above stay the default for a role not listed.
	Roles map[string]rulesCheckerRolePin `json:"roles,omitempty"`
}

// rulesCheckerRolePin is one role's pin in the pin file.
type rulesCheckerRolePin struct {
	Pack              string   `json:"pack,omitempty"`
	DefinitionDigest  string   `json:"definition_digest,omitempty"`
	DefinitionDigests []string `json:"definition_digests,omitempty"`
}

var Hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pinRulesChecker reads a pin file and pins its checker in p: the command,
// the SHA-256 of its executable, the timeout and, when given, the rulesets'
// definition digests. An empty path removes the checker.
func pinRulesChecker(p *Profile, path string) error {
	if path == "" {
		p.RulesChecker = ProfileRulesChecker{}
		return nil
	}
	raw, err := ReadInput(path)
	if err != nil {
		return err
	}
	var pin rulesCheckerPin
	if err = DecodeJSONAs("--rules-checker "+path, raw, &pin); err != nil {
		return err
	}
	if len(pin.Command) == 0 || !filepath.IsAbs(pin.Command[0]) {
		return InputError(`--rules-checker needs {"command": ["/absolute/path/to/checker", "arg", ...]}: the first element an absolute path`)
	}
	if _, err = RulesTimeout(pin.Timeout); err != nil {
		return err
	}
	digests, err := pinnedDigestSet("--rules-checker", pin.DefinitionDigest, pin.DefinitionDigests)
	if err != nil {
		return err
	}
	var roles map[string]ProfileRulesRole
	for role, rp := range pin.Roles {
		if !slices.Contains(RulesRoles, role) {
			return InputError("--rules-checker roles keys must be " + strings.Join(RulesRoles, " or ") + ", not " + role)
		}
		ds, err := pinnedDigestSet("--rules-checker roles."+role, rp.DefinitionDigest, rp.DefinitionDigests)
		if err != nil {
			return err
		}
		if rp.Pack == "" && len(ds) == 0 {
			return InputError("--rules-checker roles." + role + " needs a pack or a definition digest")
		}
		if roles == nil {
			roles = map[string]ProfileRulesRole{}
		}
		roles[role] = ProfileRulesRole{Pack: rp.Pack}.withDigests(ds)
	}
	sum, err := trustedExecutableDigest(pin.Command[0])
	if err != nil {
		return err
	}
	p.RulesChecker = ProfileRulesChecker{Command: pin.Command, SHA256: sum, Timeout: pin.Timeout, Roles: roles}
	// One digest is stored as it was before a set could be pinned, so the
	// profile file reads the same.
	if len(digests) == 1 {
		p.RulesChecker.DefinitionDigest = digests[0]
	} else {
		p.RulesChecker.DefinitionDigests = digests
	}
	return nil
}

// pinnedDigestSet merges a pin's single digest and digest set into one
// deduplicated set, refusing anything but 64 lowercase hex characters.
func pinnedDigestSet(flag, one string, many []string) ([]string, error) {
	var digests []string
	if one != "" {
		if !Hex64.MatchString(one) {
			return nil, InputError(flag + " definition_digest must be 64 lowercase hex characters")
		}
		digests = append(digests, one)
	}
	for _, d := range many {
		switch {
		case slices.Contains(digests, d):
		case !Hex64.MatchString(d):
			return nil, InputError(flag + " definition_digests must each be 64 lowercase hex characters")
		default:
			digests = append(digests, d)
		}
	}
	return digests, nil
}

func RulesTimeout(value string) (time.Duration, error) {
	if value == "" {
		return rulesTimeoutDefault, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 || d > rulesTimeoutMax {
		return 0, InputError("the rules checker's timeout must be a duration above 0 and at most 60s, such as 10s")
	}
	return d, nil
}

// trustedExecutableDigest is the SHA-256 of an executable under a trusted
// plugin root, refused anywhere else (verifyTrustedPath).
func trustedExecutableDigest(path string) (string, error) {
	checked, err := CheckedTrustedPath(path)
	if err != nil {
		return "", InputError("the rules checker must live under a trusted plugin root (" + strings.Join(trustedPluginRoots(), ", ") + "): " + err.Error())
	}
	// Read the checked path, not path: a link swapped after the check
	// reaches nothing hashed.
	f, err := os.Open(checked)
	if err != nil {
		return "", InputError("the rules checker cannot be read: " + err.Error())
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
