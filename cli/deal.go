package cli

import (
	"regexp"
	"time"
)

// `capsulectl deal` is the check an agent's host calls before the agent pays,
// books, signs or shares on a user's behalf. It is advisory: it holds an
// action only where the host runs it from a pre-action hook; otherwise a
// skipped check shows in the report. Every step is sealed as a
// Capsule in the profile's SQLite store and appended to its checkpointed log
// before anything is compared or shown:
//
//	snapshot -> seal -> diff -> show -> approval -> seal approval -> act
//
// The seed that signs these Capsules lives on the same machine as the agent,
// so the trail is tamper-evident (a later edit or deletion is detectable), not
// non-repudiation (it does not prove the user, rather than the machine, said
// something).

// dealClock is the time source for sealed timestamps; tests replace it.
var DealClock = func() time.Time { return time.Now().UTC() }

var DealIDPattern = regexp.MustCompile(`^deal-[0-9a-f]{16}$`)

// dealLogID names a deal's own checkpointed log. Each deal is its own log in
// the profile's SQLite file, so a report proves exactly one deal's steps and
// discloses nothing about any other deal.
func DealLogID(dealID string) string { return "deal/" + dealID }
