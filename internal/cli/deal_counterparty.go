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
// A counterparty is known by mechanical identities only: an email address,
// a phone number, a marketplace profile id, a relay address, and the
// merchant's registrable domain. A name is not an identity: two sellers can
// share one, and one seller can write it many ways. See sameParty for how
// two sets of identities are read as one party.
type dealCounterpartyMemory struct {
	// dealt lists the other deals, each with its counterparty's identities.
	dealt []dealPartyDeal
	// told lists every disclosure: the recipient's identities, the class and
	// when.
	told []dealPartyTold
}

type dealPartyDeal struct {
	keys []string
	deal string
}

type dealPartyTold struct {
	keys      []string
	class, at string
}

// perPartyKinds identify one party. A domain does not when the party is one
// seller among many on a platform: every marketplace stranger shares
// facebook.com.
var perPartyKinds = []string{"email", "phone", "profile_id", "relay_address"}

func perParty(keys []string) []string {
	var out []string
	for _, k := range keys {
		if slices.Contains(perPartyKinds, k[:strings.Index(k, ":")]) {
			out = append(out, k)
		}
	}
	return out
}

func sharesKey(a, b []string) bool {
	return slices.ContainsFunc(a, func(k string) bool { return slices.Contains(b, k) })
}

// sameParty reads two sets of identities as one party. When either side has
// a per-party identity (an email, a phone, a profile id, a relay address),
// only those count: a stranger on the same platform is a stranger. The
// registrable domain counts only when it is the sole identity on both sides,
// as for a merchant known by its website.
func sameParty(a, b []string) bool {
	pa, pb := perParty(a), perParty(b)
	if len(pa) > 0 || len(pb) > 0 {
		return sharesKey(pa, pb)
	}
	return sharesKey(a, b)
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
	m := &dealCounterpartyMemory{}
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
			if id != dealID {
				m.dealt = append(m.dealt, dealPartyDeal{keys: counterpartyKeys(ev.Open.Who), deal: id})
			}
		case kind == "disclosure" && ev.Disclosure != nil:
			keys := disclosureRecipientKeys(*ev.Disclosure, opened[id])
			for _, f := range ev.Disclosure.Fields {
				m.told = append(m.told, dealPartyTold{keys: keys, class: f.Class, at: ev.At})
			}
		}
	}
	return m, rows.Err()
}

// disclosureRecipientKeys are the identities of whoever a disclosure went
// to: the named recipient, else the deal's counterparty.
func disclosureRecipientKeys(d dealDisclosure, counterparty dealWho) []string {
	var keys []string
	if d.Who != nil {
		keys = counterpartyKeys(*d.Who)
	}
	if d.To == "other" {
		return keys
	}
	return append(keys, counterpartyKeys(counterparty)...)
}

// firstTime reports whether no other deal was opened with this party.
func (m *dealCounterpartyMemory) firstTime(keys []string) bool {
	return len(keys) == 0 || !slices.ContainsFunc(m.dealt, func(d dealPartyDeal) bool { return sameParty(keys, d.keys) })
}

// toldBefore returns when class was first disclosed to this party, or "".
func (m *dealCounterpartyMemory) toldBefore(keys []string, class string) string {
	first := ""
	if len(keys) == 0 {
		return first
	}
	for _, t := range m.told {
		if t.class == class && sameParty(keys, t.keys) && (first == "" || t.at < first) {
			first = t.at
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
// approval did not name, and the rule: class_not_checked when that check
// named what it was about to give, first_disclosure_unchecked when it named
// nothing and the class is a first telling to this recipient (a check made
// without `disclosing` never stands in for the first-time pause).
func uncheckedClass(events []sealedEvent, approval string, fields []dealDisclosureField, memory *dealCounterpartyMemory, keys []string) (class, rule string) {
	snap := approvingSnapshot(events, approval)
	if snap == nil {
		return "", ""
	}
	for _, f := range fields {
		switch {
		case len(snap.Disclosing) > 0 && !slices.Contains(snap.Disclosing, f.Class):
			return f.Class, "class_not_checked"
		case len(snap.Disclosing) == 0 && memory.toldBefore(keys, f.Class) == "":
			return f.Class, "first_disclosure_unchecked"
		}
	}
	return "", ""
}

// approvingSnapshot is the snapshot of the check an approval answered.
func approvingSnapshot(events []sealedEvent, approval string) *dealSnapshot {
	byID := map[string]dealEvent{}
	for _, se := range events {
		byID[se.CapsuleID] = se.Event
	}
	a := byID[approval].Approval
	if a == nil {
		return nil
	}
	ck := byID[a.Check].Check
	if ck == nil {
		return nil
	}
	return byID[ck.Snapshot].Snapshot
}
