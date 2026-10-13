package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A buying task in the shape users run it: ask broadly, see a shortlist,
// take the agent's recommendation or not, then buy. The original request is
// sealed when the task starts, before any search; the deal opens at the
// final review and cites it.

const (
	taskAsked     = "need a new chain for my commuter bike, 8 speed - the old one snapped on the way to work"
	taskWhat      = "an 8-speed bike chain"
	taskUse       = "my commuter bike"
	taskPicked    = "actually the Example Cycle Works one, order it"
	taskRecommend = "8-speed chain at Example Bike Depot, $31.95"
)

var taskShortlist = []string{taskRecommend, "8-speed chain at Example Cycle Works, $34.99", "8-speed chain at Example Spokes, $38.50"}

func taskInput(t *testing.T, budget string) string {
	t.Helper()
	in := map[string]any{"verbatim": taskAsked, "what": taskWhat, "use": taskUse}
	if budget != "" {
		var b map[string]any
		require.NoError(t, json.Unmarshal([]byte(budget), &b))
		in["budget_max"] = b
	}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	return writeJSON(t, string(raw))
}

const taskOpen = `{"type": "purchase", "channel": "web",
 "intent": {"verbatim": "` + taskPicked + `", "asked": {"item": "8-speed chain", "quantity": 1}, "allowed": ["pay"]},
 "who": {"name": "Example Cycle Works", "domain": "cycleworks.example"},
 "terms": {"item": "8-speed chain", "quantity": 1, "price_minor": 3499, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": true}}`

// taskDeal runs the task: the intent first (when task is set), then the
// final review: open, the pick, the check and the payment.
func taskDeal(t *testing.T, budget string, withTask bool) (dealID, taskID string) {
	t.Helper()
	args := []string{"open", "--input", writeJSON(t, taskOpen)}
	if withTask {
		task := dealRun(t, "task", "new", "--input", taskInput(t, budget))
		taskID = task["task_id"].(string)
		assert.Regexp(t, `^task-[0-9a-f]{16}$`, taskID)
		args = append(args, "--task", taskID)
	}
	dealID = dealRun(t, args...)["deal_id"].(string)
	pick, err := json.Marshal(map[string]any{"verbatim": taskPicked, "asked": map[string]any{"item": "8-speed chain"},
		"allowed": []string{"pay"}, "shown": taskShortlist, "recommended": taskRecommend})
	require.NoError(t, err)
	dealRun(t, "note", "--deal", dealID, "--kind", "intent", "--input", writeJSON(t, string(pick)))
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action": "pay", "amount_minor": 3499, "authorized_max_minor": 3499,
 "who": {"name": "Example Cycle Works", "domain": "cycleworks.example"},
 "terms": {"item": "8-speed chain", "quantity": 1, "price_minor": 3499, "currency": "USD"}, "recourse": {"rail": "card", "refundable": true}}`))
	require.Equal(t, true, check["proceed"], check)
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t,
		`{"action": "pay", "amount_minor": 3499, "currency": "USD", "payee": "Example Cycle Works", "rail": "card", "reference": "ORDER-88"}`))
	return dealID, taskID
}

type taskPage struct {
	html     string
	bundle   map[string]any
	openings map[string]any
	email    map[string]any
}

func taskReport(t *testing.T, dealID string) taskPage {
	t.Helper()
	dir := t.TempDir()
	page, bundle, eml := filepath.Join(dir, "receipt.html"), filepath.Join(dir, "bundle.json"), filepath.Join(dir, "receipt.eml")
	out := dealRun(t, "report", "--deal", dealID, "--html", page, "--bundle", bundle, "--email", eml)
	html := string(mustRead(t, page))
	var data struct {
		Openings map[string]any `json:"openings"`
	}
	require.NoError(t, json.Unmarshal(pageData(t, []byte(html)), &data))
	var b map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, bundle), &b))
	result, err := verifyWithDirectory(t, bundle, "")
	require.NoError(t, err, result)
	return taskPage{html: html, bundle: b, openings: data.Openings, email: out["email"].(map[string]any)}
}

// The task's scripted replay: the receipt's "What you asked" is the request
// as given when the task started, then what the user picked, with the
// shortlist shown and the agent's recommendation; the copy verifies.
func TestTheOriginalRequestIsSealedBeforeTheDealAndShownFirst(t *testing.T) {
	dealFixture(t)
	dealID, taskID := taskDeal(t, "", true)

	// The deal's baseline cites the task intent by its record digest.
	records, _ := exportRecords(t, dealID)
	refs := records[0]["x-deal-v0"].(map[string]any)["refs"].([]any)
	require.Len(t, refs, 1)
	cites := refs[0].(map[string]any)
	assert.Equal(t, "cites", cites["rel"])

	r := taskReport(t, dealID)
	assert.Equal(t, "checked", r.openings["task_intent"])
	assert.Equal(t, []any{true}, r.openings["picks"])
	report := dealReportOf(r.bundle)
	section := report["task_intent"].(map[string]any)
	assert.Equal(t, taskID, section["task_id"])
	assert.Equal(t, cites["digest"], section["record_digest"])
	assert.Equal(t, taskAsked, section["verbatim_opening"].(map[string]any)["text"], "verbatim, not trimmed or normalized")
	var lines []string
	for _, f := range section["fields"].([]any) {
		lines = append(lines, f.(map[string]any)["line"].(string))
	}
	assert.Equal(t, []string{"What: " + taskWhat, "For: " + taskUse}, lines)
	// The record itself carries no plaintext: commitments and classes only.
	record, err := json.Marshal(section["record"])
	require.NoError(t, err)
	for _, private := range []string{taskAsked, taskWhat, taskUse} {
		assert.NotContains(t, string(record), private)
	}
	assert.Contains(t, string(record), `"class":"what"`)

	// The pick: the user's words, the shortlist shown, the recommendation.
	picks := report["pick_openings"].([]any)
	require.Len(t, picks, 1)
	pick := picks[0].(map[string]any)
	assert.Equal(t, taskPicked, pick["verbatim"].(map[string]any)["text"])
	assert.Equal(t, taskRecommend, pick["recommended"].(map[string]any)["text"])
	var shown []string
	require.NoError(t, json.Unmarshal([]byte(pick["shown"].(map[string]any)["text"].(string)), &shown))
	assert.Equal(t, taskShortlist, shown)

	assert.Contains(t, r.html, "<title>Deal report: “"+taskAsked+"”</title>", "the title quotes the original request")
	text := r.email["text"].(string)
	assert.Contains(t, text, `What you asked: "`+taskAsked+`"`)
	assert.Contains(t, text, `You picked: "`+taskPicked+`"`)
	assert.NotContains(t, text, dealNoTaskIntentNote)
}

// A deal opened with no task intent says so plainly and shows its opening
// words as what it has; it never shows them as the original request.
func TestADealWithNoTaskIntentSaysSo(t *testing.T) {
	dealFixture(t)
	dealID, _ := taskDeal(t, "", false)
	records, _ := exportRecords(t, dealID)
	assert.NotContains(t, records[0]["x-deal-v0"], "refs")
	r := taskReport(t, dealID)
	assert.Equal(t, "none", r.openings["task_intent"])
	assert.NotContains(t, dealReportOf(r.bundle), "task_intent")
	text := r.email["text"].(string)
	assert.Contains(t, text, `What you asked: "`+taskPicked+`"`+"\n"+dealNoTaskIntentNote)
	assert.NotContains(t, text, "You picked:")
}

// u252 hook 1: the task bounds read the cited intent's budget_max with the
// holder's openings; without them, a bound is not checked, never satisfied.
func TestTaskBoundsReadTheBudget(t *testing.T) {
	dealFixture(t)
	dealID, _ := taskDeal(t, `{"amount_minor": 4000, "currency": "usd"}`, true)
	r := taskReport(t, dealID)
	baseline, section := taskBaseline(t, r.bundle)
	record := section["record"].(map[string]any)

	state, bounds := taskBounds(baseline, []map[string]any{record}, section)
	require.Equal(t, "read", state)
	require.Len(t, bounds, 3)
	assert.Equal(t, taskBound{Class: "budget_max", State: "available", Value: `{"amount_minor":4000,"currency":"USD"}`,
		Budget: &dealTaskBudget{AmountMinor: 4000, Currency: "USD"}}, bounds[2])

	state, bounds = taskBounds(baseline, []map[string]any{record}, map[string]any{})
	require.Equal(t, "read", state)
	for _, b := range bounds {
		assert.Equal(t, "not_checked", b.State, b.Class)
	}
	assert.Contains(t, r.html, "Budget: at most $40.00")
}

// u252 hook 2: one byte of an opening changed fails its commitment, for the
// request and for a stated limit; the page is not written over it.
func TestATamperedTaskOpeningFails(t *testing.T) {
	dealFixture(t)
	dealID, _ := taskDeal(t, `{"amount_minor": 4000, "currency": "USD"}`, true)
	r := taskReport(t, dealID)
	baseline, section := taskBaseline(t, r.bundle)
	record := section["record"].(map[string]any)

	words := section["verbatim_opening"].(map[string]any)
	words["text"] = strings.Replace(taskAsked, "commuter", "Commuter", 1)
	state, _ := taskBounds(baseline, []map[string]any{record}, section)
	assert.Equal(t, "failed", state)
	_, err := dealPageOpenings(r.bundle)
	require.ErrorContains(t, err, "the words you asked when the task started do not match")

	words["text"] = taskAsked
	budget := section["fields"].([]any)[2].(map[string]any)
	budget["value"] = `{"amount_minor":4001,"currency":"USD"}`
	state, bounds := taskBounds(baseline, []map[string]any{record}, section)
	assert.Equal(t, "read", state)
	assert.Equal(t, "failed", bounds[2].State)
	_, err = dealPageOpenings(r.bundle)
	require.ErrorContains(t, err, "(budget_max)")
}

// u252 hooks 3 and 5: with no task intent, the bounds are not available; a
// citation of a record that is not there is unresolved, never a pass.
func TestTaskBoundsWithoutAResolvedIntent(t *testing.T) {
	dealFixture(t)
	dealID, _ := taskDeal(t, "", false)
	records, _ := exportRecords(t, dealID)
	state, bounds := taskBounds(records[0], records, nil)
	assert.Equal(t, "not_available", state)
	assert.Empty(t, bounds)

	cited, _ := taskDeal(t, "", true)
	records, _ = exportRecords(t, cited)
	state, bounds = taskBounds(records[0], records, nil)
	assert.Equal(t, "unresolved", state, "the deal's own log does not hold the task's record")
	assert.Empty(t, bounds)

	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--task", "task-0000000000000000", "--input", writeJSON(t, taskOpen))
	require.ErrorContains(t, err, "unknown task: task-0000000000000000")
}

// A deal whose cited task intent this device no longer holds says so on its
// page, and shows none.
func TestAnUnresolvedTaskIntentIsReportedNotShown(t *testing.T) {
	dealFixture(t)
	dealID, taskID := taskDeal(t, "", true)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `DELETE FROM deal_steps WHERE deal_id=?`, taskID)
	require.NoError(t, err)
	require.NoError(t, s.close())

	r := taskReport(t, dealID)
	assert.Equal(t, "unresolved", r.openings["task_intent"])
	section := dealReportOf(r.bundle)["task_intent"].(map[string]any)
	assert.Equal(t, false, section["resolved"])
	assert.NotContains(t, section, "verbatim_opening")
	assert.Contains(t, r.email["text"], dealUnresolvedTaskIntentNote)
}

// u252 hook 4: a copy shared with the counterparty carries none of the
// request, the constraints, the shortlist or the recommendation.
func TestASharedCopyCarriesNoTaskIntentPlaintext(t *testing.T) {
	dealFixture(t)
	dealID, _ := taskDeal(t, `{"amount_minor": 4000, "currency": "USD"}`, true)
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		b, raw := sharedCopy(t, dealID, audience, "the shop")
		for _, private := range append([]string{taskAsked, taskWhat, taskUse, `"amount_minor":4000`, "Example Bike Depot", "Example Spokes"}, taskShortlist...) {
			assert.NotContains(t, raw, private, audience)
		}
		report := dealReportOf(b)
		assert.NotContains(t, report, "task_intent", audience)
		assert.NotContains(t, report, "pick_openings", audience)
	}
}

// The task's own log is one record, its root: listed as a task, refused as a
// deal.
func TestATaskIsItsOwnLog(t *testing.T) {
	dealFixture(t)
	task := dealRun(t, "task", "new", "--input", taskInput(t, ""))
	taskID := task["task_id"].(string)
	out, err := invoke(t, "", "--profile", "deal", "deal", "list", "--json")
	require.NoError(t, err, out)
	var rows []any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	assert.Equal(t, "task", row["kind"])
	assert.Equal(t, taskAsked, row["summary"].(map[string]any)["what"])
	_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", taskID)
	require.Error(t, err)

	for _, bad := range []string{`{"verbatim": "  "}`, `{"verbatim": "x", "budget_max": {"amount_minor": 5, "currency": "dollars"}}`, `{"verbatim": "x", "budget": 5}`} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "task", "new", "--input", writeJSON(t, bad))
		require.Error(t, err, bad)
	}
	_, err = invoke(t, "", "--profile", "deal", "deal", "open", "--input", writeJSON(t,
		strings.Replace(taskOpen, `"allowed": ["pay"]}`, `"allowed": ["pay"], "shown": ["a", "b"]}`, 1)))
	require.ErrorContains(t, err, "go on the user's pick")
}

// The profile checker accepts a task's log and a baseline that cites one.
func TestTheProfileCheckerReadsATaskIntent(t *testing.T) {
	dealFixture(t)
	dealID, _ := taskDeal(t, `{"amount_minor": 4000, "currency": "USD"}`, true)
	_, export := exportRecords(t, dealID)
	checkProfile(t, export)
	r := taskReport(t, dealID)
	_, section := taskBaseline(t, r.bundle)
	raw, err := json.Marshal([]any{section["record"]})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "task.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	checkProfile(t, path)

	// A task id is random hex: one that is all digits is still an id, never
	// a phone number (the checker's personal_data stage exempts it).
	digits := map[string]any{}
	for k, v := range section["record"].(map[string]any) {
		digits[k] = v
	}
	block := map[string]any{}
	for k, v := range digits["x-deal-v0"].(map[string]any) {
		block[k] = v
	}
	block["deal_id"] = "task-2026101212345678"
	digits["x-deal-v0"] = block
	raw, err = json.Marshal([]any{digits})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	checkProfile(t, path)
}

// taskBaseline is a copy's baseline record (as disclosed) and its report's
// task_intent section.
func taskBaseline(t *testing.T, b map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	report := dealReportOf(b)
	step := report["asked_step"].(string)
	d := b["disclosures"].(map[string]any)[step].(map[string]any)
	return d["agent_input"].(map[string]any), report["task_intent"].(map[string]any)
}

// What a reader sees under "What you asked", rendered in Chrome
// (CAPSULECTL_CHROME): the original request and its limits, then what the
// user picked, with the shortlist and the recommendation; or, with no task
// intent, the plain line saying none was sealed, and never a fake one.
func TestPresentationWhatYouAsked(t *testing.T) {
	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	chrome := startHeadlessChrome(t, binary)
	asked := func(t *testing.T, dealID string) []string {
		t.Helper()
		page := filepath.Join(t.TempDir(), "receipt.html")
		dealRun(t, "report", "--deal", dealID, "--html", page)
		p := chrome.open(t, "file://"+page, presentationViews[1])
		var lines []string
		p.eval(t, `(() => {
  const out = [];
  let on = false;
  for (const e of document.querySelectorAll("#deal > *")) {
    if (e.tagName === "H2") on = e.textContent === "What you asked";
    if (!on) continue;
    out.push(e.tagName === "DETAILS" ? e.querySelector("summary").textContent : e.textContent);
  }
  return out;
})()`, &lines)
		return lines
	}

	dealFixture(t)
	withTask, _ := taskDeal(t, `{"amount_minor": 4000, "currency": "USD"}`, true)
	assert.Equal(t, []string{
		"What you asked",
		"“" + taskAsked + "”",
		"What: " + taskWhat, "For: " + taskUse, "Budget: at most $40.00",
		"✓ These are your exact words, sealed when the task started, before any search; the deal cites that record. Checked when this page was built.",
		"You picked",
		"“" + taskPicked + "”",
		"✓ These are the exact words sealed when the deal opened, checked when this page was built.",
		"Options shown: " + strings.Join(taskShortlist, " · "),
		"The agent recommended: " + taskRecommend,
		"You chose: “" + taskPicked + "”",
	}, asked(t, withTask)[:12])

	without, _ := taskDeal(t, "", false)
	lines := asked(t, without)
	assert.Equal(t, []string{
		"What you asked",
		"No record of your original request was sealed before this deal opened, so it is not shown. Below are your words when the deal opened.",
		"“" + taskPicked + "”",
	}, lines[:3])
	assert.NotContains(t, lines, "You picked")
}

// A copy that carries another task's intent, valid and with openings that
// recompute against its own record, but not the one the deal's baseline
// cites, refuses the page: the cited digest is the only binding.
func TestACopyCarryingAnotherTasksIntentIsRefused(t *testing.T) {
	dealFixture(t)
	dealID, _ := taskDeal(t, "", true)
	otherID, _ := taskDeal(t, `{"amount_minor": 4000, "currency": "USD"}`, true)
	r, other := taskReport(t, dealID), taskReport(t, otherID)
	_, section := taskBaseline(t, r.bundle)
	_, swapped := taskBaseline(t, other.bundle)
	require.NotEqual(t, section["record_digest"], swapped["record_digest"])
	for k, v := range swapped {
		section[k] = v
	}
	_, err := dealPageOpenings(r.bundle)
	require.ErrorContains(t, err, "the task intent the copy carries is not the one the deal cites")
}
