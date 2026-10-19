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
	if pin.DefinitionDigest != "" && !Hex64.MatchString(pin.DefinitionDigest) {
		return InputError("--rules-checker definition_digest must be 64 lowercase hex characters")
	}
	var digests []string
	if pin.DefinitionDigest != "" {
		digests = append(digests, pin.DefinitionDigest)
	}
	for _, d := range pin.DefinitionDigests {
		switch {
		case slices.Contains(digests, d):
		case !Hex64.MatchString(d):
			return InputError("--rules-checker definition_digests must each be 64 lowercase hex characters")
		default:
			digests = append(digests, d)
		}
	}
	sum, err := trustedExecutableDigest(pin.Command[0])
	if err != nil {
		return err
	}
	p.RulesChecker = ProfileRulesChecker{Command: pin.Command, SHA256: sum, Timeout: pin.Timeout}
	// One digest is stored as it was before a set could be pinned, so the
	// profile file reads the same.
	if len(digests) == 1 {
		p.RulesChecker.DefinitionDigest = digests[0]
	} else {
		p.RulesChecker.DefinitionDigests = digests
	}
	return nil
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
	if err := VerifyTrustedPath(path); err != nil {
		return "", InputError("the rules checker must live under a trusted plugin root (" + strings.Join(trustedPluginRoots(), ", ") + "): " + err.Error())
	}
	f, err := os.Open(path)
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
