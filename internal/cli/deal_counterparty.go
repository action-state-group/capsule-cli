package cli

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// The counterparty memory: who the user has dealt with before, across
// deals, and what the agent has told each of them. It is read from the local
// store's own steps (every deal's opening and every sealed disclosure), so
// it never drifts from them, and it never leaves the device: a check seals
// only rule tokens from it, never a name or a value.
//
// A counterparty is known by mechanical identities only: the merchant's
// registrable domain, an email address, a phone number, a marketplace
// profile id, a relay address. A name is not an identity: two sellers can
// share one, and one seller can write it many ways.
type dealCounterpartyMemory struct {
	// dealt maps an identity to the other deals opened with it.
	dealt map[string][]string
	// told maps an identity to the classes disclosed to it, each with the
	// first time it happened.
	told map[string]map[string]string
}

// counterpartyKeys are a counterparty's mechanical identities.
func counterpartyKeys(w dealWho) []string {
	var keys []string
	add := func(kind, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		n, err := normalizeID(kind, v)
		if err != nil {
			return
		}
		if kind == "domain" {
			if apex, err := publicsuffix.EffectiveTLDPlusOne(n); err == nil {
				n = apex
			}
		}
		keys = append(keys, kind+":"+n)
	}
	add("domain", w.Domain)
	add("email", w.Email)
	add("phone", w.Phone)
	add("profile_id", w.ProfileID)
	add("relay_address", w.RelayAddress)
	return keys
}

// counterpartyMemory reads the memory from every deal in the local store.
// dealID is the deal being checked: its own opening is not an earlier
// dealing, but its own earlier disclosures are earlier tellings.
func (s *dealSession) counterpartyMemory(ctx context.Context, dealID string) (_ *dealCounterpartyMemory, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT deal_id, kind, local FROM deal_steps WHERE kind IN ('open', 'disclosure') ORDER BY deal_id, n`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	m := &dealCounterpartyMemory{dealt: map[string][]string{}, told: map[string]map[string]string{}}
	opened := map[string]dealWho{}
	for rows.Next() {
		var id, kind, local string
		if err = rows.Scan(&id, &kind, &local); err != nil {
			return nil, err
		}
		var ev dealEvent
		if err = json.Unmarshal([]byte(local), &ev); err != nil {
			return nil, err
		}
		switch {
		case kind == "open" && ev.Open != nil:
			opened[id] = ev.Open.Who
			if id == dealID {
				continue
			}
			for _, k := range counterpartyKeys(ev.Open.Who) {
				if !slices.Contains(m.dealt[k], id) {
					m.dealt[k] = append(m.dealt[k], id)
				}
			}
		case kind == "disclosure" && ev.Disclosure != nil:
			d := ev.Disclosure
			var keys []string
			if d.Who != nil {
				keys = counterpartyKeys(*d.Who)
			}
			if d.To == "counterparty" {
				keys = append(keys, counterpartyKeys(opened[id])...)
			}
			for _, k := range keys {
				if m.told[k] == nil {
					m.told[k] = map[string]string{}
				}
				for _, f := range d.Fields {
					if _, ok := m.told[k][f.Class]; !ok {
						m.told[k][f.Class] = ev.At
					}
				}
			}
		}
	}
	return m, rows.Err()
}

// firstTime reports whether no other deal was opened with any of keys.
func (m *dealCounterpartyMemory) firstTime(keys []string) bool {
	for _, k := range keys {
		if len(m.dealt[k]) > 0 {
			return false
		}
	}
	return true
}

// toldBefore returns when class was first disclosed to any of keys, or "".
func (m *dealCounterpartyMemory) toldBefore(keys []string, class string) string {
	first := ""
	for _, k := range keys {
		if at, ok := m.told[k][class]; ok && (first == "" || at < first) {
			first = at
		}
	}
	return first
}

// dealRecipient is what the counterparty memory says about the party a
// check is about: whether the user has dealt with them before, and, for a
// share, which classes are told to them for the first time and which again.
type dealRecipient struct {
	Name      string   `json:"name"`
	FirstTime bool     `json:"first_time"`
	First     []string `json:"first_disclosures,omitempty"`
	Repeat    []string `json:"repeat_disclosures,omitempty"`
}

// recipientName is how the card names them: their name, else their first
// identifier.
func recipientName(w dealWho) string {
	if strings.TrimSpace(w.Name) != "" {
		return w.Name
	}
	for _, f := range whoFields(w) {
		if strings.TrimSpace(f.value) != "" {
			return f.value
		}
	}
	return "them"
}

// disclosureAction is the point of no return that covers telling classes, or
// an error naming the class that does not fit.
func disclosureAction(classes []string) (string, error) {
	action := ""
	for _, c := range classes {
		a, ok := disclosureClasses[c]
		if !ok {
			return "", inputError("disclosing names a class that is not one of name, phone, email, home_address, address, pickup_location, other_contact, credential, verification_code, payment_card, id_document: " + c)
		}
		if action != "" && a != action {
			return "", inputError("disclosing mixes contact details and credentials: check each with its own action")
		}
		action = a
	}
	return action, nil
}

func sortedClasses(classes []string) []string {
	out := slices.Clone(classes)
	sort.Strings(out)
	return slices.Compact(out)
}

// uncheckedClass returns a class a disclosure gave that the check behind its
// approval did not name, when that check named what it was about to give.
func uncheckedClass(events []sealedEvent, approval string, fields []dealDisclosureField) string {
	byID := map[string]dealEvent{}
	for _, se := range events {
		byID[se.CapsuleID] = se.Event
	}
	a := byID[approval].Approval
	if a == nil {
		return ""
	}
	ck := byID[a.Check].Check
	if ck == nil {
		return ""
	}
	snap := byID[ck.Snapshot].Snapshot
	if snap == nil || len(snap.Disclosing) == 0 {
		return ""
	}
	for _, f := range fields {
		if !slices.Contains(snap.Disclosing, f.Class) {
			return f.Class
		}
	}
	return ""
}
