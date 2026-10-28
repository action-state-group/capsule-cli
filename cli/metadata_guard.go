package cli

import (
	"errors"
	"regexp"
	"slices"
)

// Host-supplied records (an agent host's execution and approval records,
// backfilled history) carry identifiers, never content. GuardMetadata is the
// one check the plugins apply to them.
var (
	metadataToken = regexp.MustCompile(`^[A-Za-z0-9._:/+-]{1,200}$`)
	metadataHost  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	panLike       = regexp.MustCompile(`\d(?:[ -]?\d){12,18}`)
	phoneLike     = regexp.MustCompile(`\+\d`)
	currencyCode  = regexp.MustCompile(`^[A-Za-z]{3}$`)
)

// GuardMetadata refuses an identifier field that is not shaped like one, or
// that carries a card-number-like run of digits or a phone-like number. The
// fields named in hosts are host names. The refusal names the field, never
// the value. Empty fields are not checked.
func GuardMetadata(fields map[string]string, hosts ...string) error {
	for name, v := range fields {
		if v == "" {
			continue
		}
		shape := metadataToken
		if slices.Contains(hosts, name) {
			shape = metadataHost
		}
		if !shape.MatchString(v) || panLike.MatchString(v) || phoneLike.MatchString(v) {
			return errors.New(name + " must be an identifier (metadata only: no card numbers, codes or form content)")
		}
	}
	return nil
}

// IsCurrencyCode reports whether s is shaped like an ISO 4217 code: three
// letters.
func IsCurrencyCode(s string) bool { return currencyCode.MatchString(s) }
