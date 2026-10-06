package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evidenceExchange is one requester (a) and one responder (b) with a book of
// one exchange, and helpers for the evidence-request round trip.
type evidenceExchange struct {
	t     *testing.T
	dir   string
	b     Profile
	bKeys bookKeys
}

func newEvidenceExchange(t *testing.T) *evidenceExchange {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	initBook(t, a)
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	return &evidenceExchange{t: t, dir: t.TempDir(), b: b, bKeys: bKeys}
}

func (x *evidenceExchange) ask(name, body string) requestResult {
	path := filepath.Join(x.dir, name+".json")
	require.NoError(x.t, os.WriteFile(path, []byte(body), 0o600))
	out, err := invoke(x.t, "", "request", "--profile", "a", "--request", path, "--responder", "b", "--output", filepath.Join(x.dir, name+".sent"))
	require.NoError(x.t, err)
	var r requestResult
	require.NoError(x.t, json.Unmarshal([]byte(out), &r))
	return r
}

func (x *evidenceExchange) respond(name string) evidencebook.Response {
	return x.respondAs(name, name)
}

// respondAs answers the request sent as name, writing the answer as out.
func (x *evidenceExchange) respondAs(name, out string) evidencebook.Response {
	_, err := invoke(x.t, "", "respond", "--profile", "b", "--request", filepath.Join(x.dir, name+".sent"), "--requester", "a", "--output", filepath.Join(x.dir, out+".response"))
	require.NoError(x.t, err)
	raw, err := os.ReadFile(filepath.Join(x.dir, out+".response"))
	require.NoError(x.t, err)
	var resp evidencebook.Response
	require.NoError(x.t, json.Unmarshal(raw, &resp))
	return resp
}

// resign re-signs an artifact response with the responder's own key, as a
// responder (or anyone holding its key) could after changing the artifact.
func (x *evidenceExchange) resign(a *evidencebook.ArtifactResponse) {
	sum := sha256.Sum256(a.Artifact)
	a.ArtifactDigest = hex.EncodeToString(sum[:])
	body, err := a.SigningBody()
	require.NoError(x.t, err)
	seed, err := hex.DecodeString(x.b.Signing.Value)
	require.NoError(x.t, err)
	a.Sig = hex.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), body))
}

func (x *evidenceExchange) record(sent requestResult, resp evidencebook.Response, extra ...string) (requestResult, error) {
	path := filepath.Join(x.dir, sent.RecordID+".recorded")
	raw, err := json.Marshal(resp)
	require.NoError(x.t, err)
	require.NoError(x.t, os.WriteFile(path, raw, 0o600))
	out, err := invoke(x.t, "", append([]string{"request", "--profile", "a", "--for", sent.RecordID, "--response", path,
		"--responder-key", hex.EncodeToString(x.bKeys.record), "--responder-checkpoint-key", hex.EncodeToString(x.bKeys.checkpoint)}, extra...)...)
	var r requestResult
	if err == nil {
		require.NoError(x.t, json.Unmarshal([]byte(out), &r))
	}
	return r, err
}

// A history answer whose bundle carries a witness receipt (re-signed by the
// responder's own key, so only the receipt can fail) is a granted artifact
// only when every receipt verifies, against the answer's anchor, under the
// requester's --witness-directory. A receipt with bogus bytes, one from a
// witness the directory does not list, or any receipt with no directory,
// records the answer as failed.
func TestRecordedAnswerChecksCarriedWitnessReceipts(t *testing.T) {
	witnessKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	_, otherKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	bogus := func(b map[string]any) {
		witnessed(t, witnessKey)(b)
		entry := b["checkpoint"].(map[string]any)["witnesses"].([]any)[0].(map[string]any)
		entry["receipt_b64"] = "Ym9ndXM="
	}
	for name, tc := range map[string]struct {
		change    func(map[string]any)
		directory []string
		want      string
	}{
		"genuine, listed":     {witnessed(t, witnessKey), []string{"--witness-directory", writeDirectory(t, rawKeyRow(testWitness, witnessKey.Public().(ed25519.PublicKey)))}, evidencebook.OutcomeArtifact},
		"genuine, no dir":     {witnessed(t, witnessKey), nil, evidencebook.OutcomeArtifactFailed},
		"genuine, other key":  {witnessed(t, witnessKey), []string{"--witness-directory", writeDirectory(t, rawKeyRow(testWitness, otherKey.Public().(ed25519.PublicKey)))}, evidencebook.OutcomeArtifactFailed},
		"bogus bytes, listed": {bogus, []string{"--witness-directory", writeDirectory(t, rawKeyRow(testWitness, witnessKey.Public().(ed25519.PublicKey)))}, evidencebook.OutcomeArtifactFailed},
		"no receipt, no dir":  {nil, nil, evidencebook.OutcomeArtifact},
	} {
		t.Run(name, func(t *testing.T) {
			x := newEvidenceExchange(t)
			sent := x.ask("history", `{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`)
			resp := x.respond("history")
			require.NotNil(t, resp.Artifact)
			if tc.change != nil {
				var bundle map[string]any
				require.NoError(t, json.Unmarshal(resp.Artifact.Artifact, &bundle))
				tc.change(bundle)
				resp.Artifact.Artifact, err = json.Marshal(bundle)
				require.NoError(t, err)
				x.resign(resp.Artifact)
			}
			got, err := x.record(sent, resp, tc.directory...)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Outcome)
		})
	}

	x := newEvidenceExchange(t)
	sent := x.ask("history", `{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`)
	_, err = invoke(t, "", "request", "--profile", "a", "--for", sent.RecordID, "--absent-until", day1.Format(time.RFC3339), "--witness-directory", writeDirectory(t))
	assert.ErrorIs(t, err, ErrInput, "--witness-directory goes with --response only")
}

// A fixed pin: the same pinned request, answered before and after the log
// grows by unrelated records and a newer checkpoint, gets the same artifact
// bytes: the pin is the anchor, never the latest checkpoint.
func TestFixedPinArtifactSurvivesLogGrowth(t *testing.T) {
	x := newEvidenceExchange(t)
	x.ask("first", `{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`)
	anchor := x.respond("first").Artifact.Anchor
	pin, err := evidencebook.ParsePin(anchor)
	require.NoError(t, err)
	pinned := `{"subject":{"kind":"full_history"},"coverage":{"expected_pin":{"root":"` + pin.Root + `","mmr_size":` + strconv.FormatUint(pin.MMRSize, 10) + `}}}`
	x.ask("pinned", pinned)
	before := x.respond("pinned").Artifact

	appendHalves(t, x.b, half{"x2", "r2", "p2"}, half{"x3", "r3", "p3"})
	x.ask("grow", `{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":`+strconv.FormatUint(bookSize(t, x.b), 10)+`}}}`)
	grown := x.respond("grow").Artifact
	require.NotEqual(t, anchor, grown.Anchor, "the log grew and a newer checkpoint was issued")

	after := x.respondAs("pinned", "pinned-again").Artifact
	t.Logf("anchors %s / %s; artifact bytes equal: %v", before.Anchor, after.Anchor, string(before.Artifact) == string(after.Artifact))
	require.Equal(t, before.Anchor, after.Anchor)
	require.Equal(t, string(before.Artifact), string(after.Artifact), "a fixed pin's artifact does not change as the log grows")
}

// A pin is a checkpoint, {root, mmr_size}: an exchange digest given as the
// pin is refused.
func TestExchangeDigestIsNotAPin(t *testing.T) {
	x := newEvidenceExchange(t)
	h := hex.EncodeToString(make([]byte, 32))
	sent, err := invoke(t, "", "request", "--profile", "a", "--request", writeJSON(t,
		`{"subject":{"kind":"exchange","digest":"`+h+`"},"coverage":{"expected_pin":"`+h+`"}}`), "--responder", "b", "--output", filepath.Join(x.dir, "half.sent"))
	t.Logf("asking with an exchange digest as the pin: err=%v out=%s", err, sent)
	require.Error(t, err, "a pin is a checkpoint {root, mmr_size}; an exchange digest is not a pin")
}
