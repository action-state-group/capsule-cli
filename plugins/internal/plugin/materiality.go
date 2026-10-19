package plugin

import (
	"github.com/action-state-group/capsule-cli/internal/cli"
)

// materialityLabel names a predicate for the card: its name and version, or
// that none was configured.
func materialityLabel(m cli.DealMateriality) string {
	if m.Digest == "none" || m.Name == "" {
		return "none: every pick asked about"
	}
	return m.Name + " " + m.Version
}

// materialityOpenings are, for each step that committed a materiality
// predicate's name and version, the step and the opening of its
// label_commitment: the nonce, name and version. Only the user's own copy
// carries them.
func materialityOpenings(events []sealedEvent) []interface{} {
	out := []interface{}{}
	for _, se := range events {
		nonce := se.Event.Nonces["materiality"]
		if nonce == "" {
			continue
		}
		var m cli.DealMateriality
		switch {
		case se.Event.Open != nil:
			m = se.Event.Open.Materiality
		case se.Event.Check != nil:
			m = se.Event.Check.Materiality
		default:
			continue
		}
		out = append(out, map[string]interface{}{"step": se.CapsuleID, "nonce": nonce, "name": m.Name, "version": m.Version})
	}
	return out
}
