package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"unicode"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Counterparty identifiers never enter a sealed record raw. A record carries
// a per-deal HMAC fingerprint of each normalized identifier, and free text
// that may contain identifiers is carried as a salted commitment. Raw values,
// the per-deal key and the commitment nonces stay in the local store. The
// construction is the x-deal-v0 profile's (skills/deal/profile/PROFILE.md §4).

var errNotNormalizable = errors.New("identifier cannot be normalized")

var legalForms = map[string]bool{"llc": true, "inc": true, "ltd": true, "corp": true, "co": true, "company": true, "gmbh": true, "llp": true, "plc": true}

var (
	phoneExtension = regexp.MustCompile(`(?i)\s*(?:ext\.?|x|#)\s*\d+\s*$`)
	nonDigit       = regexp.MustCompile(`\D`)
	payeeIsPhone   = regexp.MustCompile(`^\s*\+?[\d\s().-]{7,}\s*$`)
	urlScheme      = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://`)
	domainPort     = regexp.MustCompile(`:\d+$`)
)

func normPhone(v string) (string, error) {
	v = strings.TrimSpace(norm.NFKC.String(v))
	if loc := phoneExtension.FindStringIndex(v); loc != nil {
		v = v[:loc[0]]
	}
	plus := strings.HasPrefix(v, "+")
	digits := nonDigit.ReplaceAllString(v, "")
	if !plus {
		switch {
		case len(digits) == 10: // v0 default region: NANP
			digits = "1" + digits
		case len(digits) == 11 && strings.HasPrefix(digits, "1"):
		default:
			return "", errNotNormalizable
		}
	}
	if len(digits) < 8 || len(digits) > 15 {
		return "", errNotNormalizable
	}
	return "+" + digits, nil
}

func normEmail(v string) (string, error) {
	v = strings.ToLower(norm.NFC.String(strings.TrimSpace(v)))
	if strings.Count(v, "@") != 1 || strings.HasPrefix(v, "@") || strings.HasSuffix(v, "@") {
		return "", errNotNormalizable
	}
	return v, nil
}

func normDomain(v string) (string, error) {
	v = strings.ToLower(norm.NFC.String(strings.TrimSpace(v)))
	v = urlScheme.ReplaceAllString(v, "")
	if i := strings.IndexAny(v, "/?#"); i >= 0 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, "@"); i >= 0 {
		v = v[i+1:]
	}
	v = strings.TrimRight(domainPort.ReplaceAllString(v, ""), ".")
	ascii, err := idna.Lookup.ToASCII(v)
	if err != nil {
		return "", errNotNormalizable
	}
	labels := strings.Split(ascii, ".")
	if len(labels) < 2 || slicesContainsEmpty(labels) {
		return "", errNotNormalizable
	}
	registrable, err := publicsuffix.EffectiveTLDPlusOne(ascii)
	if err != nil {
		return "", errNotNormalizable
	}
	out, err := idna.Lookup.ToUnicode(registrable)
	if err != nil {
		return "", errNotNormalizable
	}
	return out, nil
}

func slicesContainsEmpty(labels []string) bool {
	for _, l := range labels {
		if l == "" {
			return true
		}
	}
	return false
}

var foldCase = cases.Fold()

func normName(v string) (string, error) {
	v = foldCase.String(norm.NFC.String(v))
	v = strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) {
			return ' '
		}
		return r
	}, v)
	toks := strings.Fields(v)
	if len(toks) > 0 && toks[0] == "the" {
		toks = toks[1:]
	}
	for len(toks) > 0 && legalForms[toks[len(toks)-1]] {
		toks = toks[:len(toks)-1]
	}
	if len(toks) == 0 {
		return "", errNotNormalizable
	}
	return strings.Join(toks, " "), nil
}

func normalizeID(kind, v string) (string, error) {
	switch kind {
	case "phone":
		return normPhone(v)
	case "email":
		return normEmail(v)
	case "domain":
		return normDomain(v)
	case "name":
		return normName(v)
	case "payee":
		switch {
		case strings.Contains(v, "@"):
			return normEmail(v)
		case payeeIsPhone.MatchString(v):
			return normPhone(v)
		default:
			return normName(v)
		}
	case "relay_address":
		return strings.ToLower(norm.NFC.String(strings.TrimSpace(v))), nil
	case "profile_id":
		out := norm.NFC.String(strings.TrimSpace(v))
		if out == "" {
			return "", errNotNormalizable
		}
		return out, nil
	default:
		return "", errNotNormalizable
	}
}

// sameID compares two raw identifiers the way their fingerprints would:
// normalized. When either side cannot be normalized, it falls back to a
// case-insensitive comparison of the trimmed text.
func sameID(kind, a, b string) bool {
	na, ea := normalizeID(kind, a)
	nb, eb := normalizeID(kind, b)
	if ea == nil && eb == nil {
		return na == nb
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func hmacSHA256(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// dealKeyFor derives the per-deal key from the store secret.
func dealKeyFor(storeSecret []byte, dealID string) []byte {
	return hmacSHA256(storeSecret, "x-deal-v0/deal-key\x00", dealID)
}

// fingerprintID is the profile fingerprint of one raw identifier, or
// errNotNormalizable, in which case the value is not recorded.
func fingerprintID(dealKey []byte, kind, raw string) (string, error) {
	n, err := normalizeID(kind, raw)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hmacSHA256(dealKey, "x-deal-v0/fp\x00", kind, "\x00", n)), nil
}

// dealCommitAlg names commitText's construction, sealed as commit_alg on
// each record: SHA-256 over the RFC 8785 (JCS) bytes of {"nonce", "text"},
// with nonce a fresh random 256-bit value (64 lowercase hex) for each
// commitment. No key: whoever holds a commitment's opening (its nonce and
// text) recomputes it, and nobody else can test a guess against it.
const dealCommitAlg = "sha256-jcs-nonce256"

// commitText is the salted commitment SHA-256(JCS({"nonce","text"})).
func commitText(nonceHex, text string) (string, error) {
	return canonical.JSONDigest(map[string]interface{}{"nonce": nonceHex, "text": text})
}

// Pre-seal scans, the same as the profile checker's personal_data and
// wording stages. A hit is a bug in the producer, so the step is not sealed.
var (
	scanExempt = regexp.MustCompile(`^([0-9a-f]{16,}|deal-[0-9a-f]+|\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}:\d{2}Z)?|[a-z0-9-]+/[a-z0-9-]+/\d+\.\d+\.\d+)$`)
	scanPhone  = regexp.MustCompile(`\+?\(?\d[\d\s().-]{6,}\d`)
	scanEmail  = regexp.MustCompile(`[^\s@]+@[^\s@]+\.[^\s@]+`)
	scanWords  = regexp.MustCompile(`[^a-z]+`)
	bannedWord = map[string]bool{
		"scam": true, "scams": true, "scammer": true, "scammers": true, "scammy": true, "fraud": true, "frauds": true,
		"fraudster": true, "fraudulent": true, "score": true, "scores": true, "scored": true, "scoring": true,
		"rating": true, "ratings": true, "reputation": true, "reputational": true, "blacklist": true,
		"blacklisted": true, "blocklist": true, "trustworthiness": true,
	}
)

// A rules checker's finding limit and value (body.rules.findings[].limit and
// .value) are schema-bounded scalars a pinned checker reports: an amount or a
// count, or a short line of them ("2000 authorised (capture 2000); 7d total
// 4000 (2000 earlier)"). By contract they never carry counterparty data. The
// general phone shape joins neighbouring amounts into a "number", so these
// are scanned by token instead: a plain amount passes, and a token shaped
// like a phone number does not.
var (
	scalarSplit = regexp.MustCompile(`[\s;,()\[\]]+`)
	scalarPlain = regexp.MustCompile(`^\d+(\.\d+)?$`)
	scalarPhone = regexp.MustCompile(`(^|\D)(\(?\d{3}\)?[\s.-]\d{3}[\s.-]\d{4}|\d{3}[.-]\d{4})(\D|$)`)
)

// scanCheckerScalar refuses a checker limit or value that is shaped like a
// phone number, or that holds the digits of a counterparty value on record.
func scanCheckerScalar(s string, localValues []string) error {
	phone := inputError("refusing to seal: the rules checker's answer would carry a raw phone number")
	if scalarPhone.MatchString(s) {
		return phone
	}
	for _, tok := range scalarSplit.Split(s, -1) {
		digits := len(nonDigit.ReplaceAllString(tok, ""))
		switch {
		case scalarPlain.MatchString(tok) && digits <= 9:
		case digits >= 7:
			return phone
		}
	}
	// A counterparty's number however it is written: by its digits, with or
	// without a country code (its last ten).
	all := nonDigit.ReplaceAllString(s, "")
	for _, raw := range localValues {
		d := nonDigit.ReplaceAllString(raw, "")
		if len(d) > 10 {
			d = d[len(d)-10:]
		}
		if len(d) >= 7 && strings.Contains(all, d) {
			return inputError("refusing to seal: the record would carry a raw counterparty identifier")
		}
	}
	return nil
}

// checkerScalars sets aside, from a record's strings, a rules checker's
// finding limits and values, which are scanned by scanCheckerScalar.
func checkerScalars(record map[string]interface{}) map[string]int {
	out := map[string]int{}
	body, _ := record["body"].(map[string]interface{})
	rules, _ := body["rules"].(map[string]interface{})
	findings, _ := rules["findings"].([]interface{})
	for _, f := range findings {
		fm, _ := f.(map[string]interface{})
		for _, k := range []string{"limit", "value"} {
			if s, ok := fm[k].(string); ok {
				out[s]++
			}
		}
	}
	return out
}

func recordStrings(v interface{}, out *[]string) {
	switch tv := v.(type) {
	case string:
		*out = append(*out, tv)
	case map[string]interface{}:
		for k, x := range tv {
			*out = append(*out, k)
			recordStrings(x, out)
		}
	case []interface{}:
		for _, x := range tv {
			recordStrings(x, out)
		}
	}
}

// scanRecord refuses a record that would carry a raw phone number, email
// address or local-store value, or a word that labels or grades someone.
func scanRecord(record map[string]interface{}, localValues []string) error {
	var strs []string
	recordStrings(record, &strs)
	scalars := checkerScalars(record)
	for _, s := range strs {
		if scalars[s] > 0 {
			scalars[s]--
			if err := scanCheckerScalar(s, localValues); err != nil {
				return err
			}
			if scanEmail.MatchString(s) {
				return inputError("refusing to seal: the record would carry a raw email address")
			}
			for _, tok := range scanWords.Split(strings.ToLower(s), -1) {
				if bannedWord[tok] {
					return inputError("refusing to seal: a record may not label or grade anyone (" + tok + ")")
				}
			}
			low := foldCase.String(s)
			for _, raw := range localValues {
				if raw != "" && strings.Contains(low, foldCase.String(raw)) {
					return inputError("refusing to seal: the record would carry a raw counterparty identifier")
				}
			}
			continue
		}
		for _, tok := range scanWords.Split(strings.ToLower(s), -1) {
			if bannedWord[tok] {
				return inputError("refusing to seal: a record may not label or grade anyone (" + tok + ")")
			}
		}
		if scanExempt.MatchString(s) {
			continue
		}
		for _, m := range scanPhone.FindAllString(s, -1) {
			if len(nonDigit.ReplaceAllString(m, "")) >= 7 {
				return inputError("refusing to seal: the record would carry a raw phone number; keep it out of claims and tokens")
			}
		}
		if scanEmail.MatchString(s) {
			return inputError("refusing to seal: the record would carry a raw email address")
		}
		low := foldCase.String(s)
		for _, raw := range localValues {
			if raw != "" && strings.Contains(low, foldCase.String(raw)) {
				return inputError("refusing to seal: the record would carry a raw counterparty identifier")
			}
		}
	}
	return nil
}
