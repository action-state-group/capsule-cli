package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/settlement"
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
	for _, dir := range []string{"."} {
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
		"say.error.cause.paused", "say.error.cause.no_answer", "say.error.cause.other"} {
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

// writePack writes a pack with the core's built-in keys and those of extra,
// changed by edit.
func writePack(t *testing.T, extra map[string]string, edit func(entries map[string]string)) string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(BuiltinWordingBytes(), &doc))
	entries := map[string]string{}
	for k, v := range doc["entries"].(map[string]any) {
		entries[k] = v.(string)
	}
	for k, v := range extra {
		entries[k] = v
	}
	edit(entries)
	doc["id"], doc["entries"] = "org.example.product.wording/v0", entries
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "pack.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

// acmeWording is a plugin's own built-in pack, holding the terms line an
// approval binds to: the core's pack holds no card.terms key of its own.
var acmeWording = map[string]string{
	"card.terms.label":          "Saying yes also agrees to {terms}.",
	"card.terms.price":          "a price of {value}",
	"card.terms.was":            "(was {value})",
	"card.terms.not_refundable": "no refunds",
	"card.terms.condition":      "{name}: {value}",
	"say.acme.done":             "Done.",
}

func acmePackJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"wording_pack_version": "aac.wording-pack/v0", "id": "org.example.acme.wording/v0", "locale": "en", "entries": acmeWording})
	require.NoError(t, err)
	return raw
}

// acmeLauncher is a capsulectl-acme launcher whose handshake carries pack.
func acmeLauncher(pack []byte) string {
	handshake := map[string]any{"name": "acme", "vendor": "ACME", "version": "1.0.0", "plugin_api": PluginAPI}
	if pack != nil {
		handshake["wording"] = json.RawMessage(pack)
	}
	info, _ := json.Marshal(handshake)
	return "#!/bin/sh\nif [ \"$1\" = \"cli-plugin-metadata\" ]; then\n  cat <<'JSON'\n" + string(info) + "\nJSON\n  exit 0\nfi\nexit 3\n"
}

// acmeProfile is a profile whose log capsulectl-acme keeps, with that
// launcher installed in a trusted root.
func acmeProfile(t *testing.T, pack []byte) Profile {
	t.Helper()
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-acme", acmeLauncher(pack), 0o755)
	p := ownedProfile(t, "jsonl", "acme")
	p.Operator = "op"
	require.NoError(t, SaveProfile(p, true))
	return p
}

func TestAProfilePinsOnlyAValidCompletePack(t *testing.T) {
	var p Profile
	complete := writePack(t, nil, func(e map[string]string) { e["say.verify.unchanged"] = "Product words." })
	require.NoError(t, pinWording(&p, complete))
	assert.Equal(t, complete, p.Wording.Path)
	assert.Regexp(t, `^[0-9a-f]{64}$`, p.Wording.SHA256)
	w, problem := ActiveWording(p)
	assert.Empty(t, problem)
	assert.Equal(t, "pinned", w.Source)
	assert.Equal(t, "Product words.", w.Line("say.verify.unchanged", nil))

	incomplete := writePack(t, nil, func(e map[string]string) { delete(e, "say.verify.unchanged") })
	assert.ErrorContains(t, pinWording(&p, incomplete), "is not complete: it lacks 1 of the built-in pack's keys, first say.verify.unchanged")
	assert.Equal(t, complete, p.Wording.Path, "a refused pin leaves the pin as it was")

	invalid := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(invalid, []byte(`{"entries":{}}`), 0o600))
	assert.ErrorContains(t, pinWording(&p, invalid), "not a valid aac.wording-pack/v0")
	assert.ErrorIs(t, pinWording(&p, invalid), ErrInput)

	require.NoError(t, pinWording(&p, ""))
	assert.Equal(t, ProfileWording{}, p.Wording)
}

// The merge gate of capsule-cli#428 step 3 (Steven's door (c) ruling): the
// terms line lives in a plugin's pack, and a pinned pack that could hide a
// term from it is still refused by the core, which reads the plugin's pack
// from its handshake in another process. A pack made from the core's pack
// alone is refused with a line naming the plugin whose keys it lacks.
func TestPinningChecksThePluginsPackAcrossProcesses(t *testing.T) {
	p := acmeProfile(t, acmePackJSON(t))

	coreOnly := writePack(t, nil, func(map[string]string) {})
	_, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--wording", coreOnly)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "lacks 6 of the capsulectl-acme plugin's keys, first card.terms.condition: a pack for this profile holds the core's keys and that plugin's")

	for key, text := range map[string]string{
		"card.terms.label":          "Saying yes also agrees.",
		"card.terms.price":          "a new price",
		"card.terms.was":            "(was)",
		"card.terms.not_refundable": " ",
		"card.terms.condition":      "{name}: {value} {extra}",
	} {
		hiding := writePack(t, acmeWording, func(e map[string]string) { e[key] = text })
		_, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--wording", hiding)
		require.ErrorIs(t, err, ErrInput, key)
		assert.Contains(t, SafeError(err), "could hide a term a yes agrees to: "+key, key)
	}
	extra := writePack(t, acmeWording, func(e map[string]string) { e["card.terms.label"] = "Saying yes also agrees to {terms}. {extra}" })
	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--wording", extra)
	assert.Contains(t, SafeError(err), "could hide a term a yes agrees to: card.terms.label")
	saved, err := LoadProfile(p.Name)
	require.NoError(t, err)
	assert.Empty(t, saved.Wording.Path, "a refused pin pins nothing")

	complete := writePack(t, acmeWording, func(e map[string]string) { e["say.acme.done"] = "All set." })
	out, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--wording", complete)
	require.NoError(t, err, out)
	saved, err = LoadProfile(p.Name)
	require.NoError(t, err)
	assert.Equal(t, complete, saved.Wording.Path)

	// doctor checks the pin the same way: a plugin release whose pack adds
	// a key leaves the pinned pack incomplete for that plugin's commands.
	out, err = invoke(t, "", "--profile", p.Name, "doctor")
	require.NoError(t, err, out)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, "pinned", report["profile"].(map[string]any)["wording"].(map[string]any)["used"])
	newer := maps.Clone(acmeWording)
	newer["card.terms.deposit"] = "a deposit of {value}"
	raw, err := json.Marshal(map[string]any{"wording_pack_version": "aac.wording-pack/v0", "id": "org.example.acme.wording/v0", "locale": "en", "entries": newer})
	require.NoError(t, err)
	writeLauncher(t, os.Getenv("CAPSULECTL_PLUGIN_ROOTS"), "capsulectl-acme", acmeLauncher(raw), 0o755)
	out, err = invoke(t, "", "--profile", p.Name, "doctor")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	wording := report["profile"].(map[string]any)["wording"].(map[string]any)
	assert.Equal(t, "built-in", wording["used"])
	assert.Contains(t, wording["issue"], "lacks 1 of the capsulectl-acme plugin's keys, first card.terms.deposit")
}

// A pin whose plugin's words cannot be read is refused: a missing plugin,
// or a pack that is not one, redefines a core key or is in another locale.
func TestPinningRefusesAPluginPackItCannotCheck(t *testing.T) {
	pack := writePack(t, acmeWording, func(map[string]string) {})
	p := acmeProfile(t, acmePackJSON(t))
	require.NoError(t, os.Remove(filepath.Join(os.Getenv("CAPSULECTL_PLUGIN_ROOTS"), "capsulectl-acme")))
	assert.ErrorContains(t, pinWording(&p, pack), "the capsulectl-acme plugin, which is not installed")

	for name, edit := range map[string]func(doc map[string]any){
		"redefines the core's key say.verify.unchanged": func(doc map[string]any) {
			doc["entries"].(map[string]any)["say.verify.unchanged"] = "Mine."
		},
		"is in locale fr, the core's in en":  func(doc map[string]any) { doc["locale"] = "fr" },
		"is not a valid aac.wording-pack/v0": func(doc map[string]any) { delete(doc, "id") },
	} {
		var doc map[string]any
		require.NoError(t, json.Unmarshal(acmePackJSON(t), &doc))
		edit(doc)
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		p := acmeProfile(t, raw)
		assert.ErrorContains(t, pinWording(&p, pack), "capsulectl-acme: the plugin's wording pack "+name, name)
	}

	// A plugin whose handshake carries no pack speaks with the core's words.
	p = acmeProfile(t, nil)
	require.NoError(t, pinWording(&p, writePack(t, nil, func(map[string]string) {})))
}

// The handshake carries the pack a plugin registered, as that plugin's
// process checks a pinned pack against it.
func TestTheHandshakeCarriesTheRegisteredPack(t *testing.T) {
	saved, savedBytes := registeredWording, registeredWordingBytes
	t.Cleanup(func() { registeredWording, registeredWordingBytes = saved, savedBytes })
	w, err := pluginPack(acmePackJSON(t))
	require.NoError(t, err)
	registeredWording, registeredWordingBytes = &w, acmePackJSON(t)
	root := NewPluginCommand(PluginMetadata{Name: "acme", Vendor: "ACME"})
	t.Cleanup(func() { runningPlugin = "" })
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"cli-plugin-metadata"})
	require.NoError(t, root.Execute())
	var info pluginInfo
	require.NoError(t, json.Unmarshal(buf.Bytes(), &info))
	got, err := pluginPack(info.Wording)
	require.NoError(t, err)
	assert.Equal(t, acmeWording, got.Entries)
	assert.Equal(t, builtinPacks{Plugin: "acme", Core: coreWording().Entries, Own: acmeWording}, processPacks())

	assert.PanicsWithValue(t, "RegisterWording: call it once, before anything speaks", func() { RegisterWording(acmePackJSON(t)) })
}

// A pinned pack that cannot be used is not used at all: the whole built-in
// pack speaks, never a mix, and the reason is given.
func TestAnUnusablePinnedPackFallsBackToTheWholeBuiltInPack(t *testing.T) {
	path := writePack(t, nil, func(e map[string]string) { e["say.verify.unchanged"] = "Product words." })
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
	pinned := func(path string) Profile {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		return Profile{Wording: ProfileWording{Path: path, SHA256: packDigest(raw)}}
	}
	stale := pinned(writePack(t, nil, func(e map[string]string) { delete(e, "say.verify.changed") }))
	w, problem = ActiveWording(stale)
	assert.Equal(t, "built-in", w.Source)
	assert.Contains(t, problem, "say.verify.changed")
	assert.Equal(t, BuiltinWording().Entries["say.verify.unchanged"], w.Line("say.verify.unchanged", nil))

	// In a plugin's process the plugin's keys count too. A pack pinned
	// before this release could word the terms line away: it is not used.
	acme := builtinPacks{Plugin: "acme", Core: coreWording().Entries, Own: acmeWording}
	hiding := pinned(writePack(t, acmeWording, func(e map[string]string) { e["card.terms.label"] = "Saying yes also agrees to {terms}. {extra}" }))
	w, problem = activeWording(hiding, acme)
	assert.Equal(t, "built-in", w.Source)
	assert.Contains(t, problem, "could hide a term a yes agrees to: card.terms.label")
	w, problem = activeWording(pinned(writePack(t, nil, func(map[string]string) {})), acme)
	assert.Equal(t, "built-in", w.Source)
	assert.Equal(t, "the pinned wording pack lacks 6 of the capsulectl-acme plugin's keys, first card.terms.condition: a pack for this profile holds the core's keys and that plugin's", problem)
	w, problem = activeWording(pinned(writePack(t, acmeWording, func(e map[string]string) { e["say.acme.done"] = "All set." })), acme)
	assert.Empty(t, problem)
	assert.Equal(t, "All set.", w.Line("say.acme.done", nil))

	out := Speak(map[string]any{}, stale, func(w Wording) Narration { return Narration{Voice: VoiceLine, Say: w.Line("say.verify.changed", nil)} })
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
	pack := writePack(t, nil, func(e map[string]string) { e["say.verify.unchanged"] = "Product words." })
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
	pack := writePack(t, nil, func(map[string]string) {})
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
