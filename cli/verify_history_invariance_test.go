package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// served is one signed answer to a history_card/1 ask for full_history under
// an expected_pin: the file `verify --history` and --also-served read. The
// artifact is any bytes; caller_invariance compares artifact digests of
// envelopes whose signature and responder verify, and says nothing of the
// card's form (card_form is its own property).
func served(t *testing.T, private ed25519.PrivateKey, anchor, nonce, artifact string) string {
	t.Helper()
	return servedAs(t, private, hex.EncodeToString(private.Public().(ed25519.PublicKey)), anchor, nonce, artifact)
}

// servedAs names the responder in the envelope, which is not the signer's
// when an answer is forged.
func servedAs(t *testing.T, private ed25519.PrivateKey, responder, anchor, nonce, artifact string) string {
	t.Helper()
	subject := evidencerequest.Subject{Kind: evidencerequest.SubjectFullHistory}
	request, err := evidencerequest.Encode(evidencerequest.MediaTypeJSON, map[string]any{
		"profile": evidencerequest.ProfileID, "derivation": stapleCard, "subject": subject.Value(),
		"coverage": map[string]any{"expected_pin": anchor}, "nonce": nonce,
	})
	require.NoError(t, err)
	envelope, err := evidencerequest.Envelope{
		Profile: evidencerequest.ProfileID, RequestDigest: evidencerequest.Digest(request), Subject: subject,
		Derivation: stapleCard, Anchor: evidencerequest.Anchor{Digest: anchor},
		ArtifactDigest: evidencerequest.Digest([]byte(artifact)), Responder: responder,
		IssuedAt: "2026-11-02T00:00:00Z", Artifact: []byte(artifact),
	}.Sign(t.Context(), ed25519Signer(private))
	require.NoError(t, err)
	reply, err := evidencerequest.Encode(evidencerequest.MediaTypeJSON, envelope.Value())
	require.NoError(t, err)
	file, err := json.Marshal(map[string]any{"history": stapleCard,
		"request": map[string]any{"media_type": evidencerequest.MediaTypeJSON, "octets_text": string(request)},
		"reply":   map[string]any{"media_type": evidencerequest.MediaTypeJSON, "octets_text": string(reply)}})
	require.NoError(t, err)
	return writeHistory(t, file)
}

func TestVerifyHistoryStapleCallerInvariance(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	holder := hex.EncodeToString(public)
	anchor, elsewhere := strings.Repeat("a", 64), strings.Repeat("b", 64)

	cases := []struct {
		name    string
		staple  func() string
		also    func() []string
		key     bool
		want    string
		finding string
	}{
		{"a lone staple compares nothing", func() string { return served(t, private, anchor, "n1", "card") }, nil, true, "NOT_CHECKED", ""},
		{"the same artifact for another caller", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string { return []string{served(t, private, anchor, "n2", "card")} }, true, "PASS", ""},
		{"a different artifact for another caller", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string { return []string{served(t, private, anchor, "n2", "another card")} }, true, "FAIL", "caller_invariance FAIL"},
		{"copies under another anchor are not comparable", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string { return []string{served(t, private, elsewhere, "n2", "another card")} }, true, "NOT_CHECKED", ""},
		{"a copy signed by another key cannot fail the staple", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string { return []string{served(t, other, anchor, "n2", "another card")} }, true, "NOT_CHECKED", ""},
		{"two copies agreeing under another anchor say nothing of the staple", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string {
				return []string{served(t, private, elsewhere, "n2", "x"), served(t, private, elsewhere, "n3", "x")}
			}, true, "NOT_CHECKED", ""},
		{"two copies differing under another anchor do not fail the staple", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string {
				return []string{served(t, private, elsewhere, "n2", "x"), served(t, private, elsewhere, "n3", "y")}
			}, true, "NOT_CHECKED", ""},
		{"a copy with the staple's own request is not another caller", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string { return []string{served(t, private, anchor, "n1", "card")} }, true, "NOT_CHECKED", ""},
		{"a staple signed by another key is not compared", func() string { return served(t, other, anchor, "n1", "card") },
			func() []string {
				return []string{served(t, private, anchor, "n2", "x"), served(t, private, anchor, "n3", "y")}
			}, true, "NOT_CHECKED", ""},
		{"a forged copy of a request does not hide a genuine conflict", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string {
				return []string{servedAs(t, other, holder, anchor, "n2", "forged"), served(t, private, anchor, "n2", "another card")}
			}, true, "FAIL", "caller_invariance FAIL"},
		{"one request answered with two artifacts", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string { return []string{served(t, private, anchor, "n1", "another card")} }, true, "FAIL", "caller_invariance FAIL"},
		{"a staple whose signature fails is not compared", func() string { return servedAs(t, other, holder, anchor, "n1", "card") },
			func() []string { return []string{served(t, private, anchor, "n2", "card")} }, true, "NOT_CHECKED", "envelope_signature"},
		{"without the counterparty key nothing is verified", func() string { return served(t, private, anchor, "n1", "card") },
			func() []string { return []string{served(t, private, anchor, "n2", "another card")} }, false, "NOT_CHECKED", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := []string{}
			if c.key {
				args = append(args, "--counterparty-key", holder)
			}
			if c.also != nil {
				for _, path := range c.also() {
					args = append(args, "--also-served", path)
				}
			}
			got := runStaple(t, c.staple(), args...)
			assert.Equal(t, c.want, got.Staples[0].Properties["caller_invariance"], got.raw)
			if c.want == "FAIL" {
				assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
			}
			if c.finding != "" {
				assert.Contains(t, strings.Join(got.Staples[0].Findings, "\n"), c.finding)
			}
		})
	}
}

// A commitment staples one answer with no request, so there is nothing to
// compare and a copy cannot be given.
func TestVerifyHistoryStapleCallerInvarianceCommitment(t *testing.T) {
	path := "testdata/goose-commitment-v1/commitment.json"
	got := runStaple(t, path, "--counterparty-key", gooseNodeKey)
	assert.Equal(t, "NOT_CHECKED", got.Staples[0].Properties["caller_invariance"])
	_, err := invoke(t, "", "verify", "--history", path, "--counterparty-key", gooseNodeKey, "--also-served", path)
	require.Error(t, err)
	_, hist := positiveHistory(t)
	_, err = invoke(t, "", "verify", "--history", hist, "--also-served", path)
	require.Error(t, err)
}

// A copy that is not a served history_card/1 is an input error, not an
// unchecked property.
func TestVerifyHistoryStapleAlsoServedUnreadable(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	staple := served(t, private, strings.Repeat("a", 64), "n1", "card")
	for name, data := range map[string]string{"not json": "x", "another history": `{"history":"standard_history/1"}`} {
		_, err := invoke(t, "", "verify", "--history", staple, "--also-served", writeHistory(t, []byte(data)))
		require.Error(t, err, name)
	}
}
