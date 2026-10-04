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
	// newProfileNoted: a paused check already said this profile had no
	// earlier counterparties.
	newProfileNoted bool
}

// newProfile reports a profile with no counterparty history at all: no other
// deal and nothing told to anyone. Every counterparty is then first-time,
// after a reinstall as much as on a first install.
func (m *dealCounterpartyMemory) newProfile() bool {
	return len(m.dealt) == 0 && len(m.told) == 0
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

// marketplaceHosts are sites where many independent sellers share one
// domain: there the domain never identifies a party, and only a per-party
// identity (a profile id, an email, a phone) does.
var marketplaceHosts = map[string]bool{
	"facebook.com": true, "craigslist.org": true, "ebay.com": true, "etsy.com": true,
	"offerup.com": true, "mercari.com": true, "poshmark.com": true, "depop.com": true,
	"vinted.com": true, "gumtree.com": true, "kijiji.ca": true, "nextdoor.com": true,
}

// counterpartyKeys are a counterparty's mechanical identities. On a
// marketplace (the deal's channel, or a known marketplace host) the domain is
// not one of them.
func counterpartyKeys(w dealWho, channel string) []string {
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
			if channel == "marketplace" || marketplaceHosts[n] {
				return
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
	rows, err := s.db.QueryContext(ctx, `SELECT deal_id, kind, local FROM deal_steps WHERE kind IN ('open', 'disclosure', 'check') ORDER BY deal_id, n`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	m := &dealCounterpartyMemory{}
	opened := map[string]*dealOpen{}
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
			opened[id] = ev.Open
			if id != dealID {
				m.dealt = append(m.dealt, dealPartyDeal{keys: counterpartyKeys(ev.Open.Who, ev.Open.Channel), deal: id})
			}
		case kind == "check" && ev.Check != nil:
			if rc := ev.Check.Recipient; rc != nil && rc.NewProfile && ev.Check.Verdict == "pause" {
				m.newProfileNoted = true
			}
		case kind == "disclosure" && ev.Disclosure != nil && opened[id] != nil:
			keys := disclosureRecipientKeys(*ev.Disclosure, *opened[id])
			for _, f := range ev.Disclosure.Fields {
				m.told = append(m.told, dealPartyTold{keys: keys, class: f.Class, at: ev.At})
			}
		}
	}
	return m, rows.Err()
}

// disclosureRecipientKeys are the identities of whoever a disclosure went
// to: the named recipient, else the deal's counterparty.
func disclosureRecipientKeys(d dealDisclosure, open dealOpen) []string {
	var keys []string
	if d.Who != nil {
		keys = counterpartyKeys(*d.Who, open.Channel)
	}
	if d.To == "other" {
		return keys
	}
	return append(keys, counterpartyKeys(open.Who, open.Channel)...)
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
	// NewProfile: this profile has no counterparty history yet, said once,
	// on its first pause.
	NewProfile bool `json:"new_profile,omitempty"`
}

// newProfileLine is said once, on the first pause from a profile with no
// counterparty history: the pause stands, and the user knows why every
// counterparty reads as first-time.
const newProfileLine = "This profile has no record of earlier counterparties yet, so everyone is first-time; this settles after your first deals"

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
	snap := approvingSnapshot(events, approval)
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

// firstTellings are the classes of a disclosure that go to this recipient
// for the first time and that no approved check named: the user has not
// nodded to them, so the disclosure must not be made.
func firstTellings(events []sealedEvent, approval string, fields []dealDisclosureField, memory *dealCounterpartyMemory, keys []string) []string {
	var named []string
	if snap := approvingSnapshot(events, approval); approval != "" && snap != nil {
		named = snap.Disclosing
	}
	var out []string
	for _, f := range fields {
		if memory.toldBefore(keys, f.Class) == "" && !slices.Contains(named, f.Class) && !slices.Contains(out, f.Class) {
			out = append(out, f.Class)
		}
	}
	return out
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

// ErrPaused is a deal step held for the user: nothing was sealed, and the
// step must not be taken until a check names it and the user approves.
var ErrPaused = errors.New("paused: this needs the user's approval first")

// sameRecipient reports whether the check behind approval was about the same
// party as disclosure d. A telling to the deal's counterparty matches a check
// about the counterparty (whatever identifiers the deal has); a telling to
// anyone else matches only a check naming that same party.
func sameRecipient(events []sealedEvent, approval string, d dealDisclosure, open dealOpen) bool {
	snap := approvingSnapshot(events, approval)
	if snap == nil {
		return false
	}
	checkOther := snap.DisclosingTo == "other" && snap.Recipient != nil
	if d.To != "other" {
		return !checkOther
	}
	if !checkOther || d.Who == nil {
		return false
	}
	return sameParty(counterpartyKeys(*d.Who, open.Channel), counterpartyKeys(*snap.Recipient, open.Channel))
}
