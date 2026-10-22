package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sale's listing post (capsule-engine#187) and the classes every live
// listing states, sealed on each offer and commit of the sale's threads
// (capsule-engine#190).

const listingText = "Example bicycle, ridden twice, no scratches. Asking $1,900."

func postingJSON(text, platform string, classes ...string) string {
	raw, _ := json.Marshal(map[string]any{"text": text, "classes": append([]string{}, classes...), "platform": platform})
	return string(raw)
}

func postCheck(t *testing.T, saleID, posting string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", saleID, "--input", writeJSON(t, `{"action":"post","posting":`+posting+`}`))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, nil
}

func postAct(t *testing.T, saleID, posting string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", saleID, "--kind", "act", "--input", writeJSON(t, `{"action":"post","posting":`+posting+`}`))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, nil
}

// post checks the listing, answers its card with the user's "post it", and
// posts it.
func post(t *testing.T, saleID, posting string) map[string]any {
	t.Helper()
	ck, err := postCheck(t, saleID, posting)
	require.NoError(t, err)
	_, err = answerCheck(t, saleID, ck["check_id"].(string), "post it", ck["card"].(string))
	require.NoError(t, err)
	done, err := postAct(t, saleID, posting)
	require.NoError(t, err)
	return done
}

// A post check always asks, on a card that quotes the exact words and names
// the classes the listing states; no rules checker runs on it. Only the
// user's own "post it" on that card authorizes the post, of exactly those
// words; edited text is a new check that asks again.
func TestAPostAlwaysAsksAndOnlyTheUsersYesPostsIt(t *testing.T) {
	dealFixture(t)
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	saleID, _ := newSale(t)
	posting := postingJSON(listingText, "https://www.Marketplace.example/listings/new", "price", "Condition")

	ck, err := postCheck(t, saleID, posting)
	require.NoError(t, err)
	assert.Equal(t, "pause", ck["verdict"])
	assert.Equal(t, false, ck["proceed"])
	assert.Equal(t, rulesNotApplicable, ck["rules"].(map[string]any)["status"], "no checker runs on a post, pinned or not")
	_, err = os.Stat(checker + ".input")
	assert.True(t, os.IsNotExist(err), "the pinned checker was not called")
	card := ck["card"].(string)
	assert.Contains(t, card, `Post this listing on marketplace.example: "`+listingText+`"?`)
	assert.Contains(t, card, "This listing states the item's condition and price")
	assert.Contains(t, card, "Why I'm asking: posting a listing always asks you first.")
	assert.Contains(t, differenceTexts(ck), "posting a listing always asks you first")
	assert.Equal(t, rulePublicPost, ck["differences"].([]any)[0].(map[string]any)["rule"])
	labels := []string{}
	for _, o := range ck["options"].([]any) {
		labels = append(labels, o.(map[string]any)["label"].(string))
	}
	assert.Equal(t, []string{"Hold", "Post it"}, labels)

	// No approval: refused at the act, nothing sealed.
	_, err = postAct(t, saleID, posting)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "no approval of the user's")
	// A card click is no approval of a post.
	_, err = answerCheck(t, saleID, ck["check_id"].(string), "", ck["card"].(string))
	require.NoError(t, err)
	_, err = postAct(t, saleID, posting)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "refusing the post: only the user's own approval, in their words (--said), on the card shown, authorizes a post. A tap on a card button alone does not")

	// Edited text is a new check, and asks again.
	edited := postingJSON(listingText+" Pickup only.", "marketplace.example", "condition", "price")
	again, err := postCheck(t, saleID, edited)
	require.NoError(t, err)
	assert.Equal(t, "pause", again["verdict"])
	// The user's own yes, in their words, on the card shown.
	_, err = answerCheck(t, saleID, again["check_id"].(string), "post it", "")
	require.ErrorIs(t, err, cli.ErrInput, "bound to the card it was given on")
	_, err = answerCheck(t, saleID, again["check_id"].(string), "post it", again["card"].(string))
	require.NoError(t, err)
	// The act carries exactly the approved words.
	_, err = postAct(t, saleID, posting)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "not the one last checked")
	done, err := postAct(t, saleID, edited)
	require.NoError(t, err)
	assert.Equal(t, false, done["unchecked"])
	assert.Equal(t, "marketplace.example", done["platform"])
	// One approval posts once.
	_, err = postAct(t, saleID, edited)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "already covered an earlier post")

	recs, path := exportSale(t, saleID)
	byType := recordsByType(recs)
	require.Len(t, byType["proposed-action/v0"], 2)
	body := byType["proposed-action/v0"][0]["body"].(map[string]any)
	assert.Equal(t, "post", body["action"])
	assert.Equal(t, "communication.publish", body["action_class"])
	p := body["posting"].(map[string]any)
	assert.Equal(t, []any{"condition", "price"}, p["classes"])
	assert.Equal(t, "marketplace.example", p["platform"])
	assert.Regexp(t, `^[0-9a-f]{64}$`, p["text_commitment"])
	assert.NotContains(t, string(mustRead(t, path)), "ridden twice", "the words only by commitment")
	require.Len(t, byType["action-record/v0"], 1)
	act := byType["action-record/v0"][0]["body"].(map[string]any)
	assert.Equal(t, "post", act["action"])
	assert.Equal(t, "communication.publish", act["action_class"])
	for _, ev := range byType["action-evaluation/v0"] {
		assert.Equal(t, rulesNotApplicable, ev["body"].(map[string]any)["rules_status"], "the typed evaluation says no checker ran")
	}
	basis := act["authority_basis"].([]any)
	require.Len(t, basis, 2)
	assert.Equal(t, "user_approval", basis[1].(map[string]any)["type"])
	checkProfile(t, path)
}

// The listing can't carry the agent's own authenticity claim: a word from
// the authenticity list outside the seller's attributed template is refused,
// whatever the labels say, naming the word and both ways out; an
// authenticity label needs the template; the template itself asks as every
// post does.
func TestAListingNeverCarriesTheAgentsAuthenticityClaim(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	for name, c := range map[string]struct {
		text    string
		classes []string
		want    string
	}{
		"genuine labelled condition": {"Genuine leather saddle, good condition.", []string{"condition"}, saysWord("Genuine")},
		"a stem in any case":         {"LEGITIMATE seller, good condition.", []string{"condition"}, saysWord("LEGITIMATE")},
		"original, with the reword":  {"Comes with its original box.", []string{"condition"}, `"comes with its original box" becomes "comes with its box"`},
		"knock-off":                  {"Not a knock-off.", []string{}, saysWord("knock-off")},
		"outside the template":       {"The seller says it's used (unverified). It's authentic.", []string{"authenticity"}, saysWord("authentic")},
		"two sentences in one":       {"The seller says it's used. Authentic too (unverified)", []string{"authenticity"}, saysWord("Authentic")},
		"a line break in the claim":  {"The seller says it's used\nAuthentic too (unverified)", []string{"authenticity"}, saysWord("Authentic")},
		"an ideographic full stop":   {"The seller says it's used。Authentic too (unverified)", []string{"authenticity"}, saysWord("Authentic")},
		"a danda":                    {"The seller says it's used।Authentic too (unverified)", []string{"authenticity"}, saysWord("Authentic")},
		"the label with no template": {"Good condition.", []string{"authenticity", "condition"}, `"The seller says … (unverified)"`},
		"no ending":                  {"The seller says it's genuine.", []string{"authenticity"}, saysWord("genuine")},
		"the claim with no label": {"Good condition. The seller says it's authentic (unverified).", []string{"condition"},
			"the listing states authenticity as the seller's claim: add authenticity to classes and check again"},
		"the label on a claim of something else": {"The seller says it was never dropped (unverified).", []string{"authenticity"},
			"a listing may state authenticity only as the seller's own claim"},
		// The template is read exactly: another case, or a look-alike letter
		// in it, is no template, so its claim is the agent's own.
		"the template in capitals":   {"THE SELLER SAYS it is genuine (UNVERIFIED)", []string{"authenticity"}, saysWord("genuine")},
		"a look-alike in the prefix": {"The sеller says it is genuine (unverified)", []string{"authenticity"}, saysWord("genuine")},
		"a look-alike in the suffix": {"The seller says it is genuine (unvеrified)", []string{"authenticity"}, saysWord("genuine")},
		"fullwidth":                  {"ｇｅｎｕｉｎｅ leather.", []string{}, saysWord("genuine")},
		"a soft hyphen":              {"Gen­uine leather.", []string{}, saysWord("Genuine")},
		"a zero-width space":         {"Gen​uine leather.", []string{}, saysWord("Genuine")},
		"a Greek alpha":              {"Not fαke.", []string{}, saysWord("fαke")},
		"an underscore":              {"tags: _genuine_", []string{}, saysWord("genuine")},
		"knock off":                  {"Not a knock off.", []string{}, saysWord("knock off")},
		"a Greek capital iota":       {"AUTHENTΙC item.", []string{}, saysWord("AUTHENTΙC")},
		"a Greek capital eta":        {"AUTΗENTIC item.", []string{}, saysWord("AUTΗENTIC")},
		"Greek capital nus":          {"GEΝUIΝE leather.", []string{}, saysWord("GEΝUIΝE")},
		"a Greek nu in original":     {"ORIGIΝAL box.", []string{}, saysWord("ORIGIΝAL")},
		"a Greek nu in knock off":    {"Not a KΝOCK OFF.", []string{}, saysWord("KΝOCK OFF")},
		"an ASCII l for I":           {"AUTHENTlC item.", []string{}, saysWord("AUTHENTlC")},
		"a Cyrillic palochka":        {"AUTHENTӀC item.", []string{}, saysWord("AUTHENTӀC")},
		"a 1 for I":                  {"GENU1NE leather.", []string{}, saysWord("GENU1NE")},
		"an I for l":                 {"Iegit seller.", []string{}, saysWord("Iegit")},
		"a 0 for O":                  {"0RIGINAL box.", []string{}, saysWord("0RIGINAL")},
		"an enclosing circle":        {"g⃝enuine leather.", []string{}, saysWord("g⃝enuine")},
		"an enclosing mark":          {"gen҈uine leather.", []string{}, saysWord("gen҈uine")},
		"a circle mid-word":          {"authen⃝tic piece.", []string{}, saysWord("authen⃝tic")},
		"a spacing mark":             {"genःuine leather.", []string{}, saysWord("genःuine")},
		"a no-break space":           {"Not a knock off.", []string{}, saysWord("knock off")},
		"an ideographic space":       {"Not a knock　off.", []string{}, saysWord("knock off")},
		"a next-line control":        {"Not a knock\u0085off.", []string{}, saysWord("knock\u0085off")},
		"a minus sign":               {"Not a knock−off.", []string{}, saysWord("knock−off")},
		"negative circled letters":   {"\U0001F156\U0001F154\U0001F15D\U0001F164\U0001F158\U0001F15D\U0001F154 leather.", []string{}, saysWord("\U0001F156\U0001F154\U0001F15D\U0001F164\U0001F158\U0001F15D\U0001F154")},
		// Squared letters have a compatibility form: they are quoted folded.
		"squared letters": {"\U0001F136\U0001F134\U0001F13D\U0001F144\U0001F138\U0001F13D\U0001F134.", []string{}, saysWord("GENUINE")},
		// A vertical stroke is read both as the letter i and as a gap.
		"pipes around":           {"|Genuine leather|", []string{}, saysWord("Genuine")},
		"a pipe before":          {"Condition: used|authentic", []string{"condition"}, saysWord("authentic")},
		"a pipe after":           {"Original|box included", []string{}, saysWord("Original")},
		"pipes in tags":          {"tags:|genuine|", []string{}, saysWord("genuine")},
		"pipes around fake":      {"|fake| watch", []string{}, saysWord("fake")},
		"a pipe in a colour":     {"Colour: blue|Original", []string{}, saysWord("Original")},
		"pipes in a size":        {"Size 9|Certified|Box", []string{}, saysWord("Certified")},
		"a pipe then certified":  {"x|certified", []string{}, saysWord("certified")},
		"a pipe in knock off":    {"Not knock|off", []string{}, saysWord("knock|off")},
		"pipes around knock off": {"a |knock off|", []string{}, saysWord("knock off")},
		"a fullwidth pipe":       {"used｜authentic", []string{}, saysWord("authentic")},
		"a dental click":         {"Originalǀbox", []string{}, saysWord("Original")},
		"a pipe for i":           {"AUTHENT|C item.", []string{}, saysWord("AUTHENT|C")},
		// A rune's look-alike is read as written, before any compatibility fold.
		"a lunate sigma":              {"AUTHENTIϲ item.", []string{}, saysWord("AUTHENTIϲ")},
		"a capital lunate sigma":      {"AUTHENTIϹ item.", []string{}, saysWord("AUTHENTIϹ")},
		"a lunate sigma first":        {"ϹERTIFIED item.", []string{}, saysWord("ϹERTIFIED")},
		"all Greek":                   {"ΑUΤΗΕΝΤΙϹ item.", []string{}, saysWord("ΑUΤΗΕΝΤΙϹ")},
		"a long s":                    {"Not ſake.", []string{}, saysWord("ſake")},
		"an ideographic zero":         {"〇riginal box.", []string{}, saysWord("〇riginal")},
		"an Arabic-Indic five":        {"Not c٥unterfeit.", []string{}, saysWord("c٥unterfeit")},
		"an extended Arabic five":     {"Not c۵unterfeit.", []string{}, saysWord("c۵unterfeit")},
		"a vertical tab in the claim": {"The seller says it's used\vAuthentic too (unverified)", []string{"authenticity"}, saysWord("Authentic")},
		"an Arabic question mark":     {"The seller says it's used؟Authentic too (unverified)", []string{"authenticity"}, saysWord("Authentic")},
		// Each stroke is read both ways at once.
		"strokes all round":   {"|AUTHENT|C|", []string{}, saysWord("AUTHENT|C")},
		"a stroke each way":   {"used|GENU|NE leather", []string{}, saysWord("GENU|NE")},
		"strokes in original": {"OR|GINAL|box included", []string{}, saysWord("OR|GINAL")},
		"a stroke both ways":  {"|GENU|NE|", []string{}, saysWord("GENU|NE")},
		"strokes in the template": {"The seller says |AUTHENT|C| (unverified)", []string{"condition"},
			"the listing states authenticity as the seller's claim: add authenticity to classes and check again"},
		"strokes after used":          {"used|AUTHENT|C", []string{}, saysWord("AUTHENT|C")},
		"strokes in tags":             {"tags:|AUTHENT|C|", []string{}, saysWord("AUTHENT|C")},
		"strokes in a colour":         {"blue|ORIG|NAL", []string{}, saysWord("ORIG|NAL")},
		"strokes after x":             {"x|GENU|NE", []string{}, saysWord("GENU|NE")},
		"strokes in a size":           {"Size 9|CERT|F|ED", []string{}, saysWord("CERT|F|ED")},
		"a danda as a gap":            {"used।Authentic", []string{}, saysWord("Authentic")},
		"a divides for i":             {"AUTHENT∣C item.", []string{}, saysWord("AUTHENT∣C")},
		"a divides as a gap":          {"used∣authentic", []string{}, saysWord("authentic")},
		"a box drawing for i":         {"AUTHENT│C item.", []string{}, saysWord("AUTHENT│C")},
		"a box drawing as a gap":      {"used│authentic", []string{}, saysWord("authentic")},
		"a halfwidth for i":           {"AUTHENT￨C item.", []string{}, saysWord("AUTHENT￨C")},
		"a halfwidth as a gap":        {"used￨authentic", []string{}, saysWord("authentic")},
		"a paseq for i":               {"AUTHENT׀C item.", []string{}, saysWord("AUTHENT׀C")},
		"a paseq as a gap":            {"used׀authentic", []string{}, saysWord("authentic")},
		"a Arabic-Indic one for i":    {"AUTHENT١C item.", []string{}, saysWord("AUTHENT١C")},
		"a Arabic-Indic one as a gap": {"used١authentic", []string{}, saysWord("authentic")},
		"a vav for i":                 {"AUTHENTוC item.", []string{}, saysWord("AUTHENTוC")},
		"a vav as a gap":              {"usedוauthentic", []string{}, saysWord("authentic")},
		"a Tifinagh yan for i":        {"AUTHENTⵏC item.", []string{}, saysWord("AUTHENTⵏC")},
		"a Tifinagh yan as a gap":     {"usedⵏauthentic", []string{}, saysWord("authentic")},
		"a Lisu i for i":              {"AUTHENTꓲC item.", []string{}, saysWord("AUTHENTꓲC")},
		"a Lisu i as a gap":           {"usedꓲauthentic", []string{}, saysWord("authentic")},
		"an en dash":                  {"Not a knock–off.", []string{}, saysWord("knock–off")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := postCheck(t, saleID, postingJSON(c.text, "marketplace.example", c.classes...))
			require.ErrorIs(t, err, cli.ErrInput)
			assert.Contains(t, err.Error(), c.want)
		})
	}
	none, err := postCheck(t, saleID, postingJSON("Example bicycle.", "marketplace.example"))
	require.NoError(t, err)
	assert.Contains(t, none["card"], "This listing doesn't state the item's condition, price or anything else your rules track")
	// Words that only start like one, and the template, ask as every post.
	for text, classes := range map[string][]string{
		"\U0001D40E\U0001D42B\U0001D422\U0001D420\U0001D422\U0001D427\U0001D41A\U0001D425\U0001D425\U0001D432 bought in 2020":             {"condition"},
		"ｏｒｉｇｉｎａｌｌｙ bought in 2020":                                                                                                       {"condition"},
		"\U0001D422\U0001D425\U0001D425\U0001D41E\U0001D420\U0001D422\U0001D42D\U0001D422\U0001D426\U0001D41A\U0001D42D\U0001D41E seller": {"condition"},
		"Originaℓℓy bought in 2020":                                                 {"condition"},
		"Iｌｌegitimate claims aside, good condition":                                 {"condition"},
		"Originally bought in 2020, good condition.":                                {"condition"},
		"Good condition. The seller says it's authentic (unverified). Pickup only.": {"authenticity", "condition"},
		"The seller says it's a genuine original (unverified)":                      {"authenticity"},
		"The seller says |AUTHENT|C| (unverified)":                                  {"authenticity"},
	} {
		ck, err := postCheck(t, saleID, postingJSON(text, "marketplace.example", classes...))
		require.NoError(t, err, text)
		assert.Equal(t, "pause", ck["verdict"])
	}
}

// Each offer and commit on a sale's thread seals listing_classes: the
// intersection over the latest post act per platform when it is checked.
// No post: absent. A re-post on the same platform replaces the earlier one,
// and checks before it keep what they sealed.
func TestAThreadsOfferCarriesTheListingsInForce(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	thread := buyerThread(t, saleID, "buyer-a.example")
	listing := func() (any, bool) {
		t.Helper()
		sealed, given, _ := ruleInputs(t, thread, offerInput)
		assert.Equal(t, sealed["listing_classes"], given["listing_classes"], "the checker is given what is sealed")
		v, ok := sealed["listing_classes"]
		return v, ok
	}
	_, ok := listing()
	assert.False(t, ok, "no post act: absent")

	setDealClock(t, "2026-09-27T18:01:00Z")
	post(t, saleID, postingJSON(listingText, "marketplace.example", "condition", "price"))
	setDealClock(t, "2026-09-27T18:02:00Z")
	v, ok := listing()
	assert.True(t, ok)
	assert.Equal(t, []any{"condition", "price"}, v)

	// A re-post without the condition replaces the listing there.
	setDealClock(t, "2026-09-27T18:03:00Z")
	post(t, saleID, postingJSON("Example bicycle. Asking $1,900.", "marketplace.example", "price"))
	setDealClock(t, "2026-09-27T18:04:00Z")
	v, _ = listing()
	assert.Equal(t, []any{"price"}, v)

	// A second platform whose listing states no price: nothing in common
	// with it but what both state.
	setDealClock(t, "2026-09-27T18:05:00Z")
	post(t, saleID, postingJSON("Example bicycle, no scratches.", "other-market.example", "condition"))
	setDealClock(t, "2026-09-27T18:06:00Z")
	v, _ = listing()
	assert.Equal(t, []any{}, v, "the listings in force state no class in common")

	// Earlier checks keep what they sealed; a share or represent carries none.
	var sealedValues []any
	for _, r := range records(t, thread) {
		if typeOf(r) == cli.TypeProposedAction {
			sealedValues = append(sealedValues, r["body"].(map[string]any)["listing_classes"])
		}
	}
	assert.Equal(t, []any{nil, []any{"condition", "price"}, []any{"price"}, []any{}}, sealedValues)
}

// Only a post's steps go on a sale's own log; a thread takes no posting.
func TestASalesLogTakesOnlyAPostsSteps(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	thread := buyerThread(t, saleID, "buyer-a.example")
	refused := func(want string, args ...string) {
		t.Helper()
		_, err := invoke(t, "", append([]string{"--profile", "deal", "deal"}, args...)...)
		require.ErrorIs(t, err, cli.ErrInput, args)
		assert.Contains(t, err.Error(), want)
	}
	refused("only a post check", "check", "--deal", saleID, "--input", writeJSON(t, offerInput))
	refused("only a post's steps", "note", "--deal", saleID, "--kind", "message", "--input", writeJSON(t, `{"from":"agent","text":"hi"}`))
	refused("only the post act", "note", "--deal", saleID, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))
	refused("posting goes with a post check", "check", "--deal", thread, "--input", writeJSON(t, `{"action":"offer","amount_minor":190000,"posting":`+postingJSON(listingText, "marketplace.example")+`}`))
	refused("posting goes with a post check", "check", "--deal", thread, "--input", writeJSON(t, `{"action":"post","posting":`+postingJSON(listingText, "marketplace.example")+`}`))
	refused("listing only", "check", "--deal", saleID, "--input", writeJSON(t, `{"action":"post","amount_minor":5,"posting":`+postingJSON(listingText, "marketplace.example")+`}`))
	refused("listing only", "check", "--deal", saleID, "--input", writeJSON(t, `{"action":"post","listing_classes":["price"],"posting":`+postingJSON(listingText, "marketplace.example")+`}`))
	refused("posting.platform", "check", "--deal", saleID, "--input", writeJSON(t, `{"action":"post","posting":`+postingJSON(listingText, "not a site")+`}`))
	refused("not a representation class", "check", "--deal", saleID, "--input", writeJSON(t, `{"action":"post","posting":`+postingJSON(listingText, "marketplace.example", "colour")+`}`))
	refused("posting.classes", "check", "--deal", saleID, "--input", writeJSON(t, `{"action":"post","posting":{"text":"x","platform":"marketplace.example"}}`))
	dealRun(t, "sale", "close", "--sale", saleID)
	refused("is closed", "check", "--deal", saleID, "--input", writeJSON(t, `{"action":"post","posting":`+postingJSON(listingText, "marketplace.example")+`}`))
	// The deal-wide listings read past the sale's own log.
	dealRun(t, "deadlines")
	out, err := invoke(t, "", "--profile", "deal", "deal", "list")
	require.NoError(t, err, out)
}

// runProfileChecker runs the profile checker on the given exports.
func runProfileChecker(t *testing.T, paths ...string) (string, error) {
	t.Helper()
	python := profilePython(t)
	if python == "" {
		t.Skip("python3 not available")
	}
	out, err := exec.Command(python, append([]string{dealProfileDir + "/check_profile.py"}, paths...)...).CombinedOutput()
	return string(out), err
}

func writeRecords(t *testing.T, recs []map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(recs)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "records.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

// In the user's own copy the profile checker recomputes each sealed
// listing_classes from the sale's log, and flags one that does not match.
// Given alone, a thread's value is a producer input.
func TestTheProfileCheckerRecomputesTheListingClasses(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	thread := buyerThread(t, saleID, "buyer-a.example")
	dealRun(t, "check", "--deal", thread, "--input", writeJSON(t, offerInput))
	setDealClock(t, "2026-09-27T18:01:00Z")
	post(t, saleID, postingJSON(listingText, "marketplace.example", "condition", "price"))
	setDealClock(t, "2026-09-27T18:02:00Z")
	dealRun(t, "check", "--deal", thread, "--input", writeJSON(t, offerInput))
	setDealClock(t, "2026-09-27T18:03:00Z")
	post(t, saleID, postingJSON("Example bicycle.", "marketplace.example"))
	setDealClock(t, "2026-09-27T18:04:00Z")
	dealRun(t, "check", "--deal", thread, "--input", writeJSON(t, offerInput))
	// A second platform, then a commit check: the intersection of the two.
	setDealClock(t, "2026-09-27T18:05:00Z")
	post(t, saleID, postingJSON("Example bicycle, no scratches, $1,900.", "other-market.example", "condition", "price"))
	setDealClock(t, "2026-09-27T18:06:00Z")
	dealRun(t, "check", "--deal", thread, "--input", writeJSON(t,
		`{"action":"commit","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`))
	setDealClock(t, "2026-09-27T18:07:00Z")
	dealRun(t, "sale", "close", "--sale", saleID)

	_, salePath := exportSale(t, saleID)
	recs, threadPath := exportRecords(t, thread)
	var sealed []any
	for _, r := range recs {
		if typeOf(r) == cli.TypeProposedAction {
			sealed = append(sealed, r["body"].(map[string]any)["listing_classes"])
		}
	}
	assert.Equal(t, []any{nil, []any{"condition", "price"}, []any{}, []any{}}, sealed, "the commit: marketplace states none, so nothing in common")
	out, err := runProfileChecker(t, salePath, threadPath)
	require.NoError(t, err, out)
	assert.Equal(t, 4, strings.Count(out, "matches the sale's listings in force then"), out)

	alone, err := runProfileChecker(t, threadPath)
	require.NoError(t, err, alone)
	assert.Equal(t, 3, strings.Count(alone, "is a producer input"), alone)

	// Beside another sale's log, the thread is not that sale's: nothing is
	// recomputed, and its values stay producer inputs.
	other, _ := newSale(t)
	_, otherPath := exportSale(t, other)
	beside, err := runProfileChecker(t, otherPath, threadPath)
	require.NoError(t, err, beside)
	assert.Equal(t, 3, strings.Count(beside, "is a producer input"), beside)
	assert.NotContains(t, beside, "matches the sale's listings")

	// A value the sale's log does not give is flagged.
	var last int
	for i, r := range recs {
		if typeOf(r) == cli.TypeProposedAction {
			last = i
		}
	}
	edited := relinked(t, recs, nil, map[int]func(map[string]any){last: func(r map[string]any) {
		r["body"].(map[string]any)["listing_classes"] = []any{"condition"}
	}})
	out, err = runProfileChecker(t, salePath, writeRecords(t, edited))
	require.Error(t, err, out)
	assert.Contains(t, out, "is ['condition'], but the sale's listings in force then state []")
}

// A post act and a thread's offer in the same second, in either order, sit
// in two logs with no finer ordering: the value is reported as not
// independently established, never as a match or a mismatch. So is one
// whose copy of the sale's log ends before the check.
func TestASameSecondListingIsNotEstablished(t *testing.T) {
	for _, postFirst := range []bool{true, false} {
		dealFixture(t)
		saleID, _ := newSale(t)
		thread := buyerThread(t, saleID, "buyer-a.example")
		setDealClock(t, "2026-09-27T18:01:00Z")
		if postFirst {
			post(t, saleID, postingJSON(listingText, "marketplace.example", "condition"))
			dealRun(t, "check", "--deal", thread, "--input", writeJSON(t, offerInput))
		} else {
			dealRun(t, "check", "--deal", thread, "--input", writeJSON(t, offerInput))
			post(t, saleID, postingJSON(listingText, "marketplace.example", "condition"))
		}
		_, early := exportSale(t, saleID)
		setDealClock(t, "2026-09-27T18:02:00Z")
		dealRun(t, "sale", "close", "--sale", saleID)
		_, salePath := exportSale(t, saleID)
		_, threadPath := exportRecords(t, thread)
		for _, sale := range []string{salePath, early} {
			out, err := runProfileChecker(t, sale, threadPath)
			require.NoError(t, err, out)
			assert.Contains(t, out, "is not independently established", out)
			assert.NotContains(t, out, "matches the sale's listings")
		}
	}
}

// The profile checker holds a sale's post to its rules: each hand edit is
// rejected.
func TestTheProfileCheckerHoldsAPost(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	post(t, saleID, postingJSON(listingText, "marketplace.example", "condition"))
	recs, path := exportSale(t, saleID)
	out, err := runProfileChecker(t, path)
	require.NoError(t, err, out)
	at := map[string]int{}
	for i, r := range recs {
		if ty, ok := r["type"].(string); ok {
			at[ty] = i
		}
	}
	for name, c := range map[string]struct {
		edit map[int]func(map[string]any)
		want string
	}{
		"a card answer": {map[int]func(map[string]any){at["action-approval/v0"]: func(r map[string]any) {
			b := r["body"].(map[string]any)
			b["authority"] = "card_answer"
			delete(b, "said_commitment")
		}}, "a card answer does not"},
		"a post that did not ask": {map[int]func(map[string]any){at["action-evaluation/v0"]: func(r map[string]any) {
			r["body"].(map[string]any)["disposition"] = "DO"
		}}, "every post check asks"},
		"another check on the sale": {map[int]func(map[string]any){at["proposed-action/v0"]: func(r map[string]any) {
			b := r["body"].(map[string]any)
			b["action"] = "offer"
			delete(b, "posting")
		}}, "checks only a listing post"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runProfileChecker(t, writeRecords(t, relinked(t, recs, nil, c.edit)))
			require.Error(t, err, out)
			assert.Contains(t, out, c.want)
		})
	}
}

// A sale bundle made after a post carries the post's records on the sale's
// log and still verifies whole.
func TestASaleBundleCarriesThePost(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	buyerThread(t, saleID, "buyer-a.example")
	post(t, saleID, postingJSON(listingText, "marketplace.example", "condition"))
	b, _ := saleBundle(t, saleID)
	verdict, findings := verifySale(t, b)
	assert.Equal(t, "VALID", verdict, findings)
	recs, path := exportSale(t, saleID)
	checkProfile(t, path)
	assert.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "thread_opened", cli.TypeProposedAction,
		cli.TypeActionEvaluation, cli.TypeActionApproval, cli.TypeActionRecord, "sale_cut"}, saleRecordTypes(recs))
}

// A post check counts in a period's checks, its rules answer not_applicable.
func TestDealListCountsAPostCheck(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	post(t, saleID, postingJSON(listingText, "marketplace.example", "condition"))
	out, err := invoke(t, "", "--profile", "deal", "deal", "list", "--since", "2026-09-27", "--until", "2026-09-28", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"not_applicable":1`)
	assert.Contains(t, out, `"pause":1`)
}

// An approval sealed with a reason (the check already answered, or
// overtaken) never authorizes a post, as it never proceeds.
func TestAPostRefusesAnApprovalSealedWithAReason(t *testing.T) {
	p := dealPosting{Text: listingText, Classes: []string{"condition"}, Platform: "marketplace.example"}
	events := []sealedEvent{
		{CapsuleID: "s", Event: dealEvent{Kind: "snapshot", Snapshot: &dealSnapshot{Action: postAction, Posting: &p}}},
		{CapsuleID: "c", Event: dealEvent{Kind: "check", Check: &dealCheckResult{Action: postAction, Snapshot: "s", Verdict: "pause", ValidUntil: "2026-09-27T18:15:00Z"}}},
		{CapsuleID: "a", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "c", Choice: "proceed", Proceed: true, Approver: "user", ShownCard: "card", Reason: approvalChanged}}},
	}
	now := time.Date(2026, 9, 27, 18, 1, 0, 0, time.UTC)
	by, reason := authorizePost(events, p, now)
	assert.Empty(t, by)
	assert.Equal(t, postSuperseded, reason)
	events[2].Event.Approval.Reason = ""
	by, _ = authorizePost(events, p, now)
	assert.Equal(t, "a", by)
}

// A newer post proposal whose check never sealed (an interrupted check)
// supersedes the earlier check and its answer, as the profile checker reads
// it; and a check of an earlier proposal than the one right before it
// authorizes nothing.
func TestAPostIsSupersededByANewerProposal(t *testing.T) {
	p := dealPosting{Text: listingText, Classes: []string{"condition"}, Platform: "marketplace.example"}
	q := dealPosting{Text: listingText + " Pickup only.", Classes: []string{"condition"}, Platform: "marketplace.example"}
	snap := func(id string, posting *dealPosting) sealedEvent {
		return sealedEvent{CapsuleID: id, Event: dealEvent{Kind: "snapshot", Snapshot: &dealSnapshot{Action: postAction, Posting: posting}}}
	}
	check := func(id, of string) sealedEvent {
		return sealedEvent{CapsuleID: id, Event: dealEvent{Kind: "check", Check: &dealCheckResult{Action: postAction, Snapshot: of, Verdict: "pause", ValidUntil: "2026-09-27T18:15:00Z"}}}
	}
	approval := sealedEvent{CapsuleID: "a", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "c", Choice: "proceed", Proceed: true, Approver: "user", ShownCard: "card"}}}
	now := time.Date(2026, 9, 27, 18, 1, 0, 0, time.UTC)

	by, _ := authorizePost([]sealedEvent{snap("s", &p), check("c", "s"), approval}, p, now)
	assert.Equal(t, "a", by)
	by, reason := authorizePost([]sealedEvent{snap("s", &p), check("c", "s"), approval, snap("s2", &q)}, p, now)
	assert.Empty(t, by)
	assert.Equal(t, postSuperseded, reason)
	// A, then B, then a check of A.
	by, _ = authorizePost([]sealedEvent{snap("s", &p), snap("s2", &q), check("c", "s"), approval}, p, now)
	assert.Empty(t, by)
}

// The profile checker agrees in the other order: proposal A, proposal B,
// then B's evaluation, approval and post is valid.
func TestTheProfileCheckerTakesTheLatestProposal(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	_, err := postCheck(t, saleID, postingJSON(listingText, "marketplace.example", "condition"))
	require.NoError(t, err)
	post(t, saleID, postingJSON(listingText+" Pickup only.", "marketplace.example", "condition"))
	recs, _ := exportSale(t, saleID)
	// Drop A's evaluation: A, B, B's evaluation, approval, post.
	var evalA int
	for i, r := range recs {
		if typeOf(r) == cli.TypeActionEvaluation {
			evalA = i
			break
		}
	}
	out, err := runProfileChecker(t, writeRecords(t, relinked(t, recs, map[int]bool{evalA: true}, nil)))
	require.NoError(t, err, out)
	assert.Contains(t, out, "ALL OK")
}

// saysWord is the guard's refusal quoting a word as written, folded.
func saysWord(w string) string { return `says "` + foldText(w) + `"` }

// listingSentenceMarksSHA256 pins testdata/sentence-marks.txt, the list
// capsule-cli#391 and capsule-engine's promise_never/1.1.0 share.
const listingSentenceMarksSHA256 = "8869f15bbf6896160f67e2d6f19a0da449a3f33d1e12d4cb71efb77c7877b9aa"

// The listing template's sentence marks are the shared fixture, code point
// for code point, in order, and each breaks a listing's template.
func TestTheListingTemplatesMarksAreTheSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/sentence-marks.txt")
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, listingSentenceMarksSHA256, hex.EncodeToString(sum[:]))
	var want []rune
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "U+") {
			continue
		}
		n, err := strconv.ParseUint(line[2:], 16, 32)
		require.NoError(t, err, line)
		want = append(want, rune(n))
	}
	assert.Len(t, want, 181)
	assert.Equal(t, want, []rune(attributedSentenceMarks), "the listing template reads the represent check's one list")
	// Every one of them breaks a listing's template.
	for _, mark := range want {
		_, err := postCheckWords("The seller says it's authentic" + string(mark) + "I checked it (unverified)")
		assert.Error(t, err, "U+%04X", mark)
	}
}

// postCheckWords is the guard on a listing labelled authenticity.
func postCheckWords(text string) (struct{}, error) {
	return struct{}{}, checkListingWords(dealPosting{Text: text, Classes: []string{"authenticity"}})
}
