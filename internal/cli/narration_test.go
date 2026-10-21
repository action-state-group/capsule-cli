package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/settlement"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The built-in pack is a valid aac.wording-pack/v0, and every value is
// plain words for the user: no ids, digests, rule ids, flag names or UTC.
func TestTheBuiltInWordingPackIsValidAndPlain(t *testing.T) {
	w := BuiltinWording()
	assert.Equal(t, WordingPackID, w.ID)
	assert.Equal(t, "en", w.Locale)
	assert.Equal(t, "built-in", w.Source)
	for key, text := range w.Entries {
		AssertPlainWords(t, key, text)
		// The neutral pack names no product, price or registry.
		for _, product := range []string{"Muse", "Buzz", "Action State", "Checkup", "registry", "pricing"} {
			assert.NotContains(t, text, product, key)
		}
	}
}

// AssertPlainWords fails on text a user should never be read: an id, a
// digest, a rule id, a flag or a UTC timestamp.
func AssertPlainWords(t *testing.T, where, text string) {
	t.Helper()
	for _, bad := range plainWordsForbidden {
		assert.NotRegexp(t, bad, text, where)
	}
}

var plainWordsForbidden = []*regexp.Regexp{
	regexp.MustCompile(`[0-9a-f]{16,}`),                 // digests and ids
	regexp.MustCompile(`\bdeal-[0-9a-z]+`),              // deal ids
	regexp.MustCompile(`\b[rR][0-9]{2}\b`),              // rule ids
	regexp.MustCompile(`--[a-z]`),                       // flags
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`), // timestamps
	regexp.MustCompile(`\bUTC\b|\d{2}:\d{2}Z`),
}

// Every key the code names in full is in the built-in pack, so no line is
// ever left out for a missing key.
func TestEveryWordingKeyTheCodeNamesIsInThePack(t *testing.T) {
	w := BuiltinWording()
	key := regexp.MustCompile(`"((?:say|card|report|action)\.[a-z_.]*[a-z_])"`)
	var missing []string
	for _, dir := range []string{".", "../../plugins/internal/plugin"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			require.NoError(t, err)
			for _, m := range key.FindAllStringSubmatch(string(src), -1) {
				if _, ok := w.Entries[m[1]]; !ok {
					missing = append(missing, f+": "+m[1])
				}
			}
		}
	}
	assert.Empty(t, missing)
	// Families the code completes at run time.
	for _, family := range []string{"say.error.before", "say.error.after", "say.error.other",
		"say.error.cause.input", "say.error.cause.conflict", "say.error.cause.pending", "say.error.cause.partial",
		"say.error.cause.paused", "say.error.cause.no_answer", "say.error.cause.other",
		"say.approval.proceed.pay", "say.approval.proceed.share_contact", "say.approval.proceed.share_credentials",
		"say.approval.proceed.commit", "say.approval.proceed.sign", "say.approval.proceed.cancel", "say.approval.proceed.represent",
		"say.approval.not_applied.answered", "say.approval.not_applied.changed", "say.approval.not_applied.replaced",
		"say.approval.not_applied.confirmed", "say.approval.not_applied.other"} {
		assert.Contains(t, w.Entries, family)
	}
}

func TestALineFillsItsPlaceholders(t *testing.T) {
	w := Wording{Entries: map[string]string{"a": "Paid {amount} to {who}.", "b": "No values."}}
	assert.Equal(t, "Paid $1.90 to Shop.", w.Line("a", V{"amount": "$1.90", "who": "Shop"}))
	assert.Equal(t, "No values.", w.Line("b", nil))
	assert.Empty(t, w.Line("a", V{"amount": "$1.90"}), "a line is never spoken half-filled")
	assert.Empty(t, w.Line("missing", nil))
	assert.Equal(t, "one\nthree", Lines("one", "", "three"))
}

// writePack writes a pack with the built-in keys, changed by edit.
func writePack(t *testing.T, edit func(entries map[string]string)) string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(BuiltinWordingBytes(), &doc))
	entries := map[string]string{}
	for k, v := range doc["entries"].(map[string]any) {
		entries[k] = v.(string)
	}
	edit(entries)
	doc["id"], doc["entries"] = "org.example.product.wording/v0", entries
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "pack.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func TestAProfilePinsOnlyAValidCompletePack(t *testing.T) {
	var p Profile
	complete := writePack(t, func(e map[string]string) { e["say.open.first"] = "Product words." })
	require.NoError(t, pinWording(&p, complete))
	assert.Equal(t, complete, p.Wording.Path)
	assert.Regexp(t, `^[0-9a-f]{64}$`, p.Wording.SHA256)
	w, problem := ActiveWording(p)
	assert.Empty(t, problem)
	assert.Equal(t, "pinned", w.Source)
	assert.Equal(t, "Product words.", w.Line("say.open.first", nil))

	incomplete := writePack(t, func(e map[string]string) { delete(e, "say.open.first") })
	assert.ErrorContains(t, pinWording(&p, incomplete), "is not complete")
	assert.Equal(t, complete, p.Wording.Path, "a refused pin leaves the pin as it was")

	invalid := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(invalid, []byte(`{"entries":{}}`), 0o600))
	assert.ErrorContains(t, pinWording(&p, invalid), "not a valid aac.wording-pack/v0")
	assert.ErrorIs(t, pinWording(&p, invalid), ErrInput)

	require.NoError(t, pinWording(&p, ""))
	assert.Equal(t, ProfileWording{}, p.Wording)
}

// A pinned pack that cannot be used is not used at all: the whole built-in
// pack speaks, never a mix, and the reason is given.
func TestAnUnusablePinnedPackFallsBackToTheWholeBuiltInPack(t *testing.T) {
	path := writePack(t, func(e map[string]string) { e["say.open.first"] = "Product words." })
	var p Profile
	require.NoError(t, pinWording(&p, path))

	changed := p
	changed.Wording.SHA256 = strings.Repeat("0", 64)
	w, problem := ActiveWording(changed)
	assert.Equal(t, "built-in", w.Source)
	assert.Contains(t, problem, "changed since it was pinned")

	gone := p
	gone.Wording.Path = filepath.Join(t.TempDir(), "gone.json")
	w, problem = ActiveWording(gone)
	assert.Equal(t, "built-in", w.Source)
	assert.Contains(t, problem, "cannot be read")

	// A release that adds a key leaves an older pack incomplete.
	older := writePack(t, func(e map[string]string) { delete(e, "say.delivered") })
	raw, err := os.ReadFile(older)
	require.NoError(t, err)
	stale := Profile{Wording: ProfileWording{Path: older, SHA256: packDigest(raw)}}
	w, problem = ActiveWording(stale)
	assert.Equal(t, "built-in", w.Source)
	assert.Contains(t, problem, "say.delivered")
	assert.Equal(t, BuiltinWording().Entries["say.open.first"], w.Line("say.open.first", nil))

	out := Speak(map[string]any{}, stale, func(w Wording) Narration { return Narration{Voice: VoiceLine, Say: w.Line("say.delivered", nil)} })
	assert.Equal(t, "built-in", out["wording"], "the result says the built-in pack spoke")
	assert.Equal(t, VoiceLine, out["voice"])
}

func TestOutputIsSilentUnlessTheCommandSpeaks(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	require.NoError(t, Output(cmd, Narrate(map[string]any{"x": 1}, Narration{Voice: VoiceLine, Say: "Hello."})))
	assert.JSONEq(t, `{"spec_version":"capsule-cli-result/v1","x":1,"voice":"line","say":"Hello."}`, buf.String())
}

func TestTheErrorObjectSpeaksOnlyWhenNothingWasPrinted(t *testing.T) {
	outputWritten.Store(0)
	t.Cleanup(func() { outputWritten.Store(0) })
	phase := ErrorPhase
	ErrorPhase = func(*cobra.Command) string { return "before" }
	t.Cleanup(func() { ErrorPhase = phase })
	cmd := &cobra.Command{Use: "check"}
	cmd.Flags().String("profile", "", "")

	var stdout, stderr bytes.Buffer
	code := exitTo(&stdout, &stderr, cmd, InputError("--deal is required"), "capsulectl-deal")
	assert.Equal(t, 2, code)
	assert.Equal(t, "capsulectl-deal: invalid input or profile configuration: --deal is required\n", stderr.String(), "the stderr line is unchanged")
	var obj map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &obj))
	assert.Equal(t, "error", obj["voice"])
	assert.Equal(t, true, obj["hold"])
	assert.Equal(t, "capsule-cli-result/v1", obj["spec_version"])
	assert.Equal(t, "invalid input or profile configuration: --deal is required", obj["error"], "the hint is for the agent, in error")
	w := BuiltinWording()
	assert.Equal(t, w.Entries["say.error.before"]+"\n"+w.Entries["say.error.cause.input"], obj["say"])
	AssertPlainWords(t, "error say", obj["say"].(string))

	// Something already printed (a result, or a text report): no object.
	outputWritten.Store(10)
	stdout.Reset()
	exitTo(&stdout, &stderr, cmd, ErrPartial, "capsulectl")
	assert.Empty(t, stdout.String())
}

func TestThePhaseAndCauseOfAnErrorPickItsLines(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	w := BuiltinWording()
	phase := ErrorPhase
	t.Cleanup(func() { ErrorPhase = phase })
	for _, tc := range []struct {
		phase string
		err   error
		cause string
	}{
		{"after", ErrConflict, "conflict"},
		{"other", ErrPending, "pending"},
		{"other", ErrPaused, "paused"},
		{"other", ErrNoAnswer, "no_answer"},
		{"other", errors.New("boom"), "other"},
	} {
		ErrorPhase = func(*cobra.Command) string { return tc.phase }
		obj := errorObject(cmd, tc.err)
		require.NotNil(t, obj)
		assert.Equal(t, w.Entries["say.error."+tc.phase]+"\n"+w.Entries["say.error.cause."+tc.cause], obj["say"], tc.cause)
	}
}

// A plugin that ran speaks for itself; one that never started wrote
// nothing, so the core speaks; an operator command is exempt.
func TestTheCoreNeverSpeaksOverAPlugin(t *testing.T) {
	cmd := &cobra.Command{Use: "deal"}
	assert.Nil(t, errorObject(cmd, &PluginExit{Code: 2}))
	assert.Nil(t, errorObject(cmd, Hint(errPluginStopped, "deal stopped before it finished (signal: killed)")))
	notStarted := errors.Join(Hint(errPluginStopped, "plugin deal could not be started"), errPluginNotStarted, os.ErrNotExist)
	assert.NotNil(t, errorObject(cmd, notStarted))

	canary := &cobra.Command{Use: "canary", Annotations: map[string]string{NarrationExempt: "true"}}
	watch := &cobra.Command{Use: "watch"}
	canary.AddCommand(watch)
	assert.Nil(t, errorObject(watch, ErrAlarm))
}

// The counted stdout hands a plugin the file itself, so it is not put
// behind a pipe.
func TestAPluginGetsTheRealStdout(t *testing.T) {
	assert.Same(t, os.Stdout, stdoutFile(Stdout{File: os.Stdout}))
	var buf bytes.Buffer
	assert.Same(t, &buf, stdoutFile(&buf))
}

func TestAVerifyResultSaysWhatItFound(t *testing.T) {
	for _, tc := range []struct {
		err  error
		key  string
		exit error
	}{
		{nil, "say.verify.unchanged", nil},
		{ErrPartial, "say.verify.unable", ErrPartial},
		{ErrBundleInvalid, "say.verify.changed", ErrBundleInvalid},
	} {
		cmd := NarrateByOutcome(&cobra.Command{Use: "verify", SilenceErrors: true, SilenceUsage: true, RunE: func(c *cobra.Command, _ []string) error {
			// Members in their own order, as verify --history writes them.
			_, err := c.OutOrStdout().Write([]byte(`{"spec_version":"capsule-cli-result/v1","z":1,"a":2}` + "\n"))
			require.NoError(t, err)
			return tc.exit
		}}, verifyNarration)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetArgs(nil)
		err := cmd.Execute()
		assert.ErrorIs(t, err, tc.err)
		assert.True(t, strings.HasPrefix(buf.String(), `{"spec_version":"capsule-cli-result/v1","z":1,"a":2,"voice":"report"`), buf.String())
		var obj map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &obj))
		assert.Equal(t, BuiltinWording().Entries[tc.key], obj["say"])
	}
	// Text is passed through untouched.
	cmd := NarrateByOutcome(&cobra.Command{Use: "t", RunE: func(c *cobra.Command, _ []string) error {
		_, err := c.OutOrStdout().Write([]byte("a table\n"))
		return err
	}}, verifyNarration)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "a table\n", buf.String())
}

func TestDoctorSpeaksOnlyWhenSomethingNeedsFixing(t *testing.T) {
	cmd := &cobra.Command{Use: "doctor"}
	assert.Equal(t, VoiceSilent, doctorNarration(cmd, map[string]any{"profile": map[string]any{"present": true}}, nil).Voice)
	one := doctorNarration(cmd, map[string]any{"profile": map[string]any{"wording": map[string]any{"issue": "x"}}}, nil)
	assert.Equal(t, VoiceLine, one.Voice)
	assert.Equal(t, BuiltinWording().Entries["say.doctor.issues.one"], one.Say)
	two := doctorNarration(cmd, map[string]any{"install_check": map[string]any{"issues": []any{"a", "b"}}}, nil)
	assert.Equal(t, "Setup check found 2 problems.", two.Say)
}

// A card renders as the lines the user reads, labels kept apart.
func TestACardRendersAsItsLines(t *testing.T) {
	w := BuiltinWording()
	text := AskText(w, Ask{Tag: "PAUSED FOR YOU", Question: "Raise your limit?", Why: []string{"first payment"},
		Reply: "words", Options: []AskOption{{Label: "Hold", Choice: "hold"}}})
	assert.Equal(t, "PAUSED FOR YOU\nRaise your limit?\nWhy I'm asking: first payment.\n"+w.Entries["card.reply.words"], text)
	assert.NotContains(t, text, "Hold")
}

// The keys are listed once, sorted, so the pack reads in order.
func TestThePackIsSorted(t *testing.T) {
	var doc struct {
		Entries json.RawMessage `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(BuiltinWordingBytes(), &doc))
	keys := regexp.MustCompile(`"([^"]+)":`).FindAllStringSubmatch(string(doc.Entries), -1)
	var names []string
	for _, k := range keys {
		names = append(names, k[1])
	}
	assert.True(t, sort.StringsAreSorted(names))
}

// A profile pins its pack with profile update --wording, and doctor names a
// pinned pack that can no longer be used.
func TestDoctorNamesAPinnedPackItCannotUse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, e := invoke(t, "", "profile", "create", "--name", "one", "--type", "mysql", "--mysql-host", "localhost", "--mysql-database", "local", "--mysql-tls", "false", "--log-id", "log-a")
	require.NoError(t, e)
	pack := writePack(t, func(e map[string]string) { e["say.open.first"] = "Product words." })
	out, e := invoke(t, "", "profile", "update", "--profile", "one", "--wording", pack)
	require.NoError(t, e, out)
	p, e := LoadProfile("one")
	require.NoError(t, e)
	assert.Equal(t, pack, p.Wording.Path)

	out, e = invoke(t, "", "--profile", "one", "doctor")
	require.NoError(t, e, out)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, "pinned", report["profile"].(map[string]any)["wording"].(map[string]any)["used"])
	assert.Equal(t, VoiceSilent, report["voice"])

	require.NoError(t, os.WriteFile(pack, []byte(`{}`), 0o600))
	out, e = invoke(t, "", "--profile", "one", "doctor")
	require.NoError(t, e, out)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	wording := report["profile"].(map[string]any)["wording"].(map[string]any)
	assert.Equal(t, "built-in", wording["used"])
	assert.Contains(t, wording["issue"], "changed since it was pinned")
	assert.Equal(t, VoiceLine, report["voice"])
	assert.Equal(t, BuiltinWording().Entries["say.doctor.issues.one"], report["say"])

	out, e = invoke(t, "", "profile", "update", "--profile", "one", "--wording", "")
	require.NoError(t, e, out)
	p, e = LoadProfile("one")
	require.NoError(t, e)
	assert.Equal(t, ProfileWording{}, p.Wording)
}

// The whole path a main takes: what the command printed is counted, and a
// failure gets an error object only when nothing was printed.
func TestRunWritesOneObjectAtMost(t *testing.T) {
	run := func(t *testing.T, body func(c *cobra.Command) error) (string, string, int) {
		t.Helper()
		root := &cobra.Command{Use: "capsulectl", SilenceErrors: true, SilenceUsage: true}
		root.PersistentFlags().String("profile", "", "")
		root.AddCommand(&cobra.Command{Use: "x", RunE: func(c *cobra.Command, _ []string) error { return body(c) }})
		root.SetArgs([]string{"x"})
		stdout, err := os.CreateTemp(t.TempDir(), "stdout")
		require.NoError(t, err)
		var stderr bytes.Buffer
		code := runTo(t.Context(), root, "capsulectl", stdout, &stderr)
		raw, err := os.ReadFile(stdout.Name())
		require.NoError(t, err)
		return string(raw), stderr.String(), code
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	out, errLine, code := run(t, func(*cobra.Command) error { return InputError("--deal is required") })
	assert.Equal(t, 2, code)
	assert.Equal(t, "capsulectl: invalid input or profile configuration: --deal is required\n", errLine)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &obj), out)
	assert.Equal(t, "error", obj["voice"])

	out, _, code = run(t, func(c *cobra.Command) error {
		require.NoError(t, Output(c, map[string]any{"verified": false}))
		return ErrPartial
	})
	assert.Equal(t, 3, code)
	assert.Equal(t, 1, strings.Count(out, "\n"), "the result alone: %s", out)
	assert.NotContains(t, out, `"voice":"error"`)

	out, _, _ = run(t, func(c *cobra.Command) error {
		_, err := c.OutOrStdout().Write([]byte("a text report\n"))
		require.NoError(t, err)
		return ErrSchemaInvalid
	})
	assert.Equal(t, "a text report\n", out)

	out, _, code = run(t, func(*cobra.Command) error { return &PluginExit{Code: 5} })
	assert.Equal(t, 5, code)
	assert.Empty(t, out, "a plugin that ran spoke for itself")
}

// When the pinned pack cannot be used, every result the built-in pack
// speaks says so, an error and a verify report among them.
func TestEveryFallbackIsMarked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, e := invoke(t, "", "profile", "create", "--name", "one", "--type", "mysql", "--mysql-host", "localhost", "--mysql-database", "local", "--mysql-tls", "false", "--log-id", "log-a")
	require.NoError(t, e)
	pack := writePack(t, func(map[string]string) {})
	_, e = invoke(t, "", "profile", "update", "--profile", "one", "--wording", pack)
	require.NoError(t, e)
	require.NoError(t, os.Remove(pack))

	cmd := &cobra.Command{Use: "check"}
	cmd.Flags().String("profile", "one", "")
	assert.Equal(t, "built-in", errorObject(cmd, InputError("x"))["wording"])
	assert.Equal(t, "built-in", verifyNarration(cmd, nil, nil).Wording)
	assert.Equal(t, "", verifyNarration(&cobra.Command{Use: "verify"}, nil, nil).Wording, "no profile: the built-in pack is the one in use")
}

func TestASettlementSaysItsWorstState(t *testing.T) {
	of := func(states ...string) settlement.Result {
		var r settlement.Result
		for _, s := range states {
			r.Settlements = append(r.Settlements, settlement.Settlement{PaymentState: s})
		}
		return r
	}
	assert.Equal(t, "say.settlement.terms_only", settlementKey(of()))
	assert.Equal(t, "say.settlement.agreed", settlementKey(of(settlement.PaymentAgreed)))
	assert.Equal(t, "say.settlement.one_side", settlementKey(of(settlement.PaymentAgreed, settlement.PaymentPayeeStated)))
	assert.Equal(t, "say.settlement.unjoined", settlementKey(of(settlement.PaymentPayerStated, settlement.PaymentUnjoined)))
	assert.Equal(t, "say.settlement.mismatch", settlementKey(of(settlement.PaymentUnjoined, settlement.PaymentMismatch, settlement.PaymentAgreed)))
}
