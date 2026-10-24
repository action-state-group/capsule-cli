package deal

import (
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A share check carries what the telling memory says about its recipient
// (capsule-engine#188): the classes already told to this same party, and
// whether this party was told anything before, in this deal or another.

func shareInput(classes string) string {
	return `{"action":"share_contact","disclosing_to":"counterparty","disclosing":[` + classes + `]}`
}

// tellings asserts what a share check sealed and gave the checker.
func tellings(t *testing.T, sealed, given map[string]any, repeat []any, toldBefore bool) {
	t.Helper()
	for _, b := range []map[string]any{sealed, given} {
		assert.Equal(t, repeat, b["repeat_disclosures"])
		assert.Equal(t, toldBefore, b["recipient_told_before"])
	}
}

// openShop opens a purchase from a merchant known by its website, where the
// user allows sharing their contact details.
func openShop(t *testing.T, domain string) string {
	t.Helper()
	return dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy the otter sticker and give them what they need","allowed":["pay","share_contact"],"max_total_minor":1000},
		"who":{"name":"Shop","domain":"`+domain+`"},
		"terms":{"item":"otter sticker","price_minor":627,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
}

func TestAShareCarriesWhatWasToldBefore(t *testing.T) {
	dealFixture(t)
	m1 := openShop(t, "redbubble.com")

	// Email to M: never told anything.
	sealed, given, _ := ruleInputs(t, m1, shareInput(`"email"`))
	tellings(t, sealed, given, []any{}, false)
	check := latestCheckID(t, m1)
	dealRun(t, "note", "--deal", m1, "--kind", "approval", "--check", check, "--choice", "proceed", "--said", "ok")
	require.Equal(t, true, dealRun(t, "note", "--deal", m1, "--kind", "disclosure", "--input", writeJSON(t,
		`{"fields":[{"class":"email","value":"`+homeEmail+`"}]}`))["approved"])

	// Again, within the first deal with M: a repeat, told before.
	sealed, given, _ = ruleInputs(t, m1, shareInput(`"email"`))
	tellings(t, sealed, given, []any{"email"}, true)
	// Phone to M: a new class, to a party told before.
	sealed, given, _ = ruleInputs(t, m1, shareInput(`"phone"`))
	tellings(t, sealed, given, []any{}, true)
	// Both: only the class told before repeats.
	sealed, given, _ = ruleInputs(t, m1, shareInput(`"phone","email"`))
	tellings(t, sealed, given, []any{"email"}, true)

	// Email to M in another deal: still a repeat.
	m2 := openShop(t, "redbubble.com")
	sealed, given, _ = ruleInputs(t, m2, shareInput(`"email"`))
	tellings(t, sealed, given, []any{"email"}, true)

	// Email to N: never told anything.
	n1 := openShop(t, "society6.com")
	sealed, given, _ = ruleInputs(t, n1, shareInput(`"email"`))
	tellings(t, sealed, given, []any{}, false)

	// An earlier deal with N that told N nothing: dealt with, never told.
	n2 := openShop(t, "society6.com")
	out := dealRun(t, "check", "--deal", n2, "--input", writeJSON(t, shareInput(`"email"`)))
	assert.Equal(t, false, out["recipient"].(map[string]any)["first_time"], "the user dealt with N before")
	sealed, given, _ = ruleInputs(t, n2, shareInput(`"email"`))
	tellings(t, sealed, given, []any{}, false)

	// A pay is not a share: neither is sealed.
	sealed, given, _ = ruleInputs(t, m1, `{"action":"pay","amount_minor":627,"authorized_max_minor":627,"terms":{"item":"otter sticker","price_minor":627},"recourse":{"rail":"card","refundable":true}}`)
	for _, b := range []map[string]any{sealed, given} {
		assert.NotContains(t, b, "repeat_disclosures")
		assert.NotContains(t, b, "recipient_told_before")
	}
}

// The memory, never the input file, says what was told: an input that
// claims a repeat is overwritten.
func TestTheTellingInputsAreNotTheAgents(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	sealed, given, _ := ruleInputs(t, id, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["email"],"telling":{"repeat":["email"],"told_before":true}}`)
	tellings(t, sealed, given, []any{}, false)
}

// A typed share check seals both in its proposed action, and the export
// validates against the typed schema.
func TestATypedShareCarriesWhatWasToldBefore(t *testing.T) {
	id := stickerDeal(t, "card", true)
	sealed, given, _ := ruleInputs(t, id, shareInput(`"email"`))
	tellings(t, sealed, given, []any{}, false)
}

// No memory, or a recipient with no identity to remember them by: both
// absent, never a fabricated "not told".
func TestNoMemoryNoTellingInputs(t *testing.T) {
	var none *dealCounterpartyMemory
	assert.Nil(t, none.telling([]string{"domain:redbubble.com"}, []string{"email"}))
	m := &dealCounterpartyMemory{told: []dealPartyTold{{keys: []string{"domain:redbubble.com"}, class: "email", at: "2026-10-01T00:00:00Z"}}}
	assert.Nil(t, m.telling(nil, []string{"email"}))
	assert.Equal(t, &dealTelling{Repeat: []string{"email"}, ToldBefore: true}, m.telling([]string{"domain:redbubble.com"}, []string{"phone", "email"}))
	assert.Equal(t, &dealTelling{Repeat: []string{}, ToldBefore: false}, m.telling([]string{"domain:society6.com"}, []string{"email"}))
}

// A share the user refused, with nothing given, is not told: the next check
// of the same class to the same party is still a first telling.
func TestARefusedShareIsNotTold(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	sealed, given, _ := ruleInputs(t, id, shareInput(`"email"`))
	tellings(t, sealed, given, []any{}, false)
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", latestCheckID(t, id), "--choice", "hold", "--said", "no, don't")
	held := heldNote(t, id, `{"fields":[{"class":"email","value":"`+homeEmail+`"}]}`)
	assert.Equal(t, []any{"email"}, held["held"], "nothing is given")

	sealed, given, _ = ruleInputs(t, id, shareInput(`"email"`))
	tellings(t, sealed, given, []any{}, false)
}

// leakEmail seals, straight into the deal, an email disclosure with no
// authority (authority none), as a step the agent took without the user's OK.
func leakEmail(t *testing.T, id string) {
	t.Helper()
	leakClasses(t, id, "email")
}

// leakClasses seals one disclosure of classes with no authority.
func leakClasses(t *testing.T, id string, classes ...string) {
	t.Helper()
	var fields []dealDisclosureField
	for _, c := range classes {
		fields = append(fields, dealDisclosureField{Class: c, Value: map[string]string{"email": homeEmail, "name": homeName, "phone": "+1 555 010 4477"}[c]})
	}
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	_, err = s.seal(t.Context(), id, events, dealEvent{Kind: "disclosure", Disclosure: &dealDisclosure{
		To: "counterparty", Fields: fields, Reason: "no check covered it", Rule: "no_check"}})
	require.NoError(t, err)
	require.NoError(t, s.close())
}

// Data given without authority (a disclosure sealed authority none) is not
// told: neither rule input counts it, and the next share still pauses. The
// card says it went before without the user's OK, never "first time".
func TestADisclosureWithoutAuthorityIsNotTold(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	leakEmail(t, id)

	sealed, given, _ := ruleInputs(t, id, shareInput(`"email"`))
	tellings(t, sealed, given, []any{}, false)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"email"`)))
	assert.Equal(t, "pause", check["verdict"], "still not told")
	assert.Contains(t, differenceTexts(check), "Your email already went to Shop without your OK")
	assert.NotContains(t, differenceTexts(check), "First time telling Shop your email")
	assert.Equal(t, []any{"email"}, check["recipient"].(map[string]any)["unapproved_disclosures"])
	card := check["card"].(string)
	assert.Contains(t, card, "Your email already went to Shop (redbubble.com) without your OK. Share it again?")
	assert.Contains(t, card, "Why I'm asking: it went out without your OK, so I can't count Shop as already told.")
	assert.NotContains(t, strings.ToLower(card), "first time")

	// Held, the note says so without "first time" either.
	held := heldNote(t, id, `{"fields":[{"class":"email","value":"`+homeEmail+`"}]}`)
	assert.Equal(t, "You haven't OK'd telling Shop your email before.", held["reason"])
}

// A true first telling keeps its words.
func TestATrueFirstTellingSaysFirstTime(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"email"`)))
	assert.Contains(t, differenceTexts(check), "First time telling Shop your email")
	assert.NotContains(t, check["card"], "already went")
	assert.Contains(t, check["card"], "Why I'm asking: first time they get your details.")
	held := heldNote(t, id, `{"fields":[{"class":"email","value":"`+homeEmail+`"}]}`)
	assert.Equal(t, "First time telling Shop your email, and no check the user approved named it", held["reason"])
}

// One class went before without the user's OK, one is new: each is said as
// what it is.
func TestALeakedClassBesideANewOne(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	leakEmail(t, id)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"email","phone"`)))
	assert.Equal(t, "pause", check["verdict"])
	texts := differenceTexts(check)
	assert.Contains(t, texts, "Your email already went to Shop without your OK")
	assert.Contains(t, texts, "First time telling Shop your phone")
	assert.NotContains(t, texts, "First time telling Shop your email")
	card := check["card"].(string)
	assert.Contains(t, card, "Your email already went to Shop (redbubble.com) without your OK. This also shares your phone, which would be new to them. Share both?")
	assert.Contains(t, card, "Why I'm asking: first time they get your phone.")
	assert.NotContains(t, card, "first time they get your details")
	assert.NotContains(t, card, "first time they get your email")
	assert.NotContains(t, card, "can't count")

	// Held, the note says which class went before and which is new.
	held := heldNote(t, id, `{"fields":[{"class":"email","value":"`+homeEmail+`"},{"class":"phone","value":"+1 555 010 4477"}]}`)
	assert.Equal(t, "You haven't OK'd telling Shop your email before. This also shares your phone, which would be new to them.", held["reason"])
}

// A deny still says what it will not do, and never "first time".
func TestADeniedShareAfterALeakNeverSaysFirstTime(t *testing.T) {
	w := cli.BuiltinWording()
	state := dealState{who: dealWho{Name: "Shop", Domain: "redbubble.com"}, intent: dealIntent{Verbatim: "buy it"}}
	snap := dealSnapshot{Action: "share_contact", Disclosing: []string{"email"}, DisclosingTo: "counterparty"}
	r := dealCheckResult{Verdict: "deny", Differences: diffs("rules_deny", "first_disclosure"),
		Recipient: &dealRecipient{Name: "Shop", FirstTime: true, Unapproved: []string{"email"}}}
	card := checkCard(w, state, snap, r, nil)
	want := cardDenial(w, snap, cli.V{"payee": "Shop", "where": " (redbubble.com)", "what": "email", "item": "", "amount": "", "description": ""})
	assert.Equal(t, want, card.Question, "the deny question wins")
	assert.NotContains(t, card.Question, "already went")
	assert.NotContains(t, strings.ToLower(strings.Join(append([]string{card.Question}, card.Why...), " ")), "first time")
}

// Several classes on either side: the lists are joined, and the question
// asks about all of them.
func TestSeveralLeakedOrNewClasses(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	leakEmail(t, id)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"email","phone","name"`)))
	card := check["card"].(string)
	assert.Contains(t, card, "Your email already went to Shop (redbubble.com) without your OK. This also shares your name and phone, which would be new to them. Share all of them?")

	other := openShop(t, "society6.com")
	leakClasses(t, other, "email", "name")
	check = dealRun(t, "check", "--deal", other, "--input", writeJSON(t, shareInput(`"email","name"`)))
	assert.Contains(t, check["card"], "Your name and email already went to Shop (society6.com) without your OK. Share them again?")
	check = dealRun(t, "check", "--deal", other, "--input", writeJSON(t, shareInput(`"email","name","phone"`)))
	assert.Contains(t, check["card"], "Your name and email already went to Shop (society6.com) without your OK. This also shares your phone, which would be new to them. Share all of them?")
}

// latestCheckID is the id of the deal's latest check record.
func latestCheckID(t *testing.T, dealID string) string {
	t.Helper()
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	events, err := s.load(t.Context(), dealID)
	require.NoError(t, err)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Event.Kind == "check" {
			return events[i].CapsuleID
		}
	}
	t.Fatal("no check")
	return ""
}

// The question names every class a yes shares, whatever reasons the card
// keeps: with a changed payee and an over-limit reason taking both Why
// slots, the new class is still on the card (Codex round 3).
func TestAMixedShareNamesEveryClassWhateverTheReasons(t *testing.T) {
	w := cli.BuiltinWording()
	state := dealState{who: dealWho{Name: "Shop", Domain: "redbubble.com"}, intent: dealIntent{Verbatim: "buy it"}}
	snap := dealSnapshot{Action: "share_contact", Disclosing: []string{"email", "phone"}, DisclosingTo: "counterparty"}
	r := dealCheckResult{Verdict: "pause", Differences: diffs("payee_or_contact_changed", "over_limit", "first_disclosure", "first_disclosure"),
		Recipient: &dealRecipient{Name: "Shop", Unapproved: []string{"email"}, First: []string{"phone"}}}
	card := checkCard(w, state, snap, r, nil)
	assert.Equal(t, "Your email already went to Shop (redbubble.com) without your OK. This also shares your phone, which would be new to them. Share both?", card.Question)
	require.Len(t, card.Why, 2)
	assert.NotContains(t, strings.Join(card.Why, " "), "phone", "the reasons above took both slots")
}

// The same with a class they already have: every class is in the question.
func TestARepeatBesideALeakIsNamedWhateverTheReasons(t *testing.T) {
	w := cli.BuiltinWording()
	state := dealState{who: dealWho{Name: "Shop", Domain: "redbubble.com"}, intent: dealIntent{Verbatim: "buy it"}}
	snap := dealSnapshot{Action: "share_contact", Disclosing: []string{"email", "name", "phone"}, DisclosingTo: "counterparty"}
	r := dealCheckResult{Verdict: "pause", Differences: diffs("payee_or_contact_changed", "over_limit", "first_disclosure", "first_disclosure"),
		Recipient: &dealRecipient{Name: "Shop", Unapproved: []string{"email"}, Repeat: []string{"name"}, First: []string{"phone"}}}
	card := checkCard(w, state, snap, r, nil)
	assert.Equal(t, "Your email already went to Shop (redbubble.com) without your OK. This also shares your name, which they already have, and your phone, which would be new to them. Share all of them?", card.Question)
	require.Len(t, card.Why, 2)
	r.Recipient.First = nil
	snap.Disclosing = []string{"email", "name"}
	card = checkCard(w, state, snap, r, nil)
	assert.Equal(t, "Your email already went to Shop (redbubble.com) without your OK. This also shares your name, which they already have. Share both?", card.Question)
}

// A held note says a new class the approved check named as new, never as
// one they already have.
func TestAHeldNoteSaysAnApprovedNewClassIsNew(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	leakPhone(t, id)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"name"`)))
	require.Equal(t, "pause", check["verdict"])
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	held := heldNote(t, id, `{"fields":[{"class":"name","value":"`+homeName+`"},{"class":"phone","value":"+1 555 010 4477"}]}`)
	assert.Equal(t, []any{"phone"}, held["held"])
	assert.Equal(t, "You haven't OK'd telling Shop your phone before. This also shares your name, which would be new to them.", held["reason"])
}

// tellWithOK checks, approves and notes a class going to the deal's
// counterparty, so it is told with authority.
func tellWithOK(t *testing.T, id, class, value string) {
	t.Helper()
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"`+class+`"`)))
	if check["verdict"] == "pause" {
		dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	}
	require.Equal(t, true, dealRun(t, "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t,
		`{"fields":[{"class":"`+class+`","value":"`+value+`"}]}`))["approved"])
}

// leakPhone seals a phone disclosure with no authority.
func leakPhone(t *testing.T, id string) { t.Helper(); leakClasses(t, id, "phone") }

// The card's question names every class in the share: those that went
// without the user's OK, those they already have and those new to them.
func TestALeakedShareNamesEveryClassItGives(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	tellWithOK(t, id, "email", homeEmail)
	leakPhone(t, id)

	// Told and leaked.
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"email","phone"`)))
	assert.Contains(t, check["card"], "Your phone already went to Shop (redbubble.com) without your OK. This also shares your email, which they already have. Share both?")

	// Told, leaked and new.
	check = dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"email","phone","name"`)))
	assert.Contains(t, check["card"], "Your phone already went to Shop (redbubble.com) without your OK. This also shares your email, which they already have, and your name, which would be new to them. Share all of them?")

	// Held, the note names them the same way.
	held := heldNote(t, id, `{"fields":[{"class":"email","value":"`+homeEmail+`"},{"class":"phone","value":"+1 555 010 4477"},{"class":"name","value":"`+homeName+`"}]}`)
	assert.Equal(t, "You haven't OK'd telling Shop your phone before. This also shares your email, which they already have, and your name, which would be new to them.", held["reason"])

	// Two leaked classes only.
	other := openShop(t, "society6.com")
	leakClasses(t, other, "email", "phone")
	check = dealRun(t, "check", "--deal", other, "--input", writeJSON(t, shareInput(`"email","phone"`)))
	card := check["card"].(string)
	assert.Contains(t, card, "Share them again?")
	for _, class := range []string{"email", "phone"} {
		assert.Contains(t, strings.SplitN(card, "\n", 3)[1], class, "the question names every class")
	}
}

// A class the approved check named that had already gone out without the
// user's OK is one they have.
func TestAHeldNoteSaysAnApprovedLeakedClassIsOneTheyHave(t *testing.T) {
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	leakClasses(t, id, "email", "phone")
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"phone"`)))
	require.Equal(t, "pause", check["verdict"])
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	held := heldNote(t, id, `{"fields":[{"class":"email","value":"`+homeEmail+`"},{"class":"phone","value":"+1 555 010 4477"}]}`)
	assert.Equal(t, []any{"email"}, held["held"])
	assert.Equal(t, "You haven't OK'd telling Shop your email before. This also shares your phone, which they already have.", held["reason"])
}

// Every state of one class given beside a held leaked phone: its clause in
// the held note, or held itself.
func TestAHeldNoteSaysEachOtherClassAsWhatItIs(t *testing.T) {
	const phone = `{"class":"phone","value":"+1 555 010 4477"}`
	approve := func(t *testing.T, id, class string) {
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"`+class+`"`)))
		require.Equal(t, "pause", check["verdict"])
		dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	}
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, id string)
		held   []any
		reason string
	}{
		{"told with OK", func(t *testing.T, id string) { tellWithOK(t, id, "name", homeName) }, []any{"phone"},
			"You haven't OK'd telling Shop your phone before. This also shares your name, which they already have."},
		{"approved, new", func(t *testing.T, id string) { approve(t, id, "name") }, []any{"phone"},
			"You haven't OK'd telling Shop your phone before. This also shares your name, which would be new to them."},
		{"approved, gone before without OK", func(t *testing.T, id string) { leakClasses(t, id, "name"); approve(t, id, "name") }, []any{"phone"},
			"You haven't OK'd telling Shop your phone before. This also shares your name, which they already have."},
		{"gone before without OK, not approved", func(t *testing.T, id string) { leakClasses(t, id, "name") }, []any{"name", "phone"},
			"You haven't OK'd telling Shop your name and phone before."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dealFixture(t)
			id := openShop(t, "redbubble.com")
			tc.setup(t, id)
			leakPhone(t, id)
			held := heldNote(t, id, `{"fields":[{"class":"name","value":"`+homeName+`"},`+phone+`]}`)
			assert.Equal(t, tc.held, held["held"])
			assert.Equal(t, tc.reason, held["reason"])
		})
	}
}

// A held first telling names the other classes the telling gives too.
func TestAHeldFirstTellingNamesTheOtherClasses(t *testing.T) {
	approve := func(t *testing.T, id, class string) {
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, shareInput(`"`+class+`"`)))
		require.Equal(t, "pause", check["verdict"])
		dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	}
	note := `{"fields":[{"class":"name","value":"` + homeName + `"},{"class":"phone","value":"+1 555 010 4477"}]}`

	// Phone went out without OK, a check naming phone is approved, a new name beside it.
	dealFixture(t)
	id := openShop(t, "redbubble.com")
	leakPhone(t, id)
	approve(t, id, "phone")
	held := heldNote(t, id, note)
	assert.Equal(t, []any{"name"}, held["held"])
	assert.Equal(t, "First time telling Shop your name, and no check the user approved named it. This also shares your phone, which they already have.", held["reason"])

	// A plain first telling beside a class told with OK.
	other := openShop(t, "society6.com")
	tellWithOK(t, other, "phone", "+1 555 010 4477")
	held = heldNote(t, other, note)
	assert.Equal(t, []any{"name"}, held["held"])
	assert.Equal(t, "First time telling Shop your name, and no check the user approved named it. This also shares your phone, which they already have.", held["reason"])
}
