package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-viewer/go/pageruntime"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewerPublicKey is the AAC viewer's producer-key/v1 acceptance rule
// (agent-action-capsule ts/src/evidence-graph-view.ts, producerPublicKeys):
// public_key must be a string of exactly 64 lowercase hex characters.
var viewerPublicKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// viewerProducerKey reads a bundle's declared producer key exactly the way
// the AAC viewer does, from the JSON bytes a page would load.
func viewerProducerKey(t *testing.T, encoded []byte) string {
	t.Helper()
	var bundle struct {
		Extensions map[string]struct {
			PublicKey interface{} `json:"public_key"`
		} `json:"extensions"`
	}
	require.NoError(t, json.Unmarshal(encoded, &bundle))
	value, _ := bundle.Extensions["producer-key/v1"].PublicKey.(string)
	if !viewerPublicKey.MatchString(value) {
		return ""
	}
	return value
}

func TestAssembleBundleDeclaresProducerKey(t *testing.T) {
	profile, key := profileFixture(t)
	store, log, root := checkpointedStore(t, key, profile.LogID)
	public := key.Public().(ed25519.PublicKey)

	plain, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root, ClosureDepth: 2, Payloads: "none"})
	require.NoError(t, err)
	_, present := plain["extensions"]
	assert.False(t, present, "no producer key, no extension")

	declared, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root, ClosureDepth: 2, Payloads: "none", ProducerKey: public})
	require.NoError(t, err)

	// The exact block the AAC fixture builder writes
	// (ts/test/helpers/derived-fixtures.ts: {"producer-key/v1": {public_key}}).
	extensions, err := canonical.JCS(declared["extensions"])
	require.NoError(t, err)
	assert.Equal(t, `{"producer-key/v1":{"public_key":"`+hex.EncodeToString(public)+`"}}`, string(extensions))

	encoded, err := json.Marshal(declared)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(public), viewerProducerKey(t, encoded))

	// Digest-covered: the declaration changes the digest a countersigner
	// signs, and the bundle still verifies.
	verified := aacbundle.VerifyBundle(declared)
	assert.Equal(t, "pass", verified.IntervalCoverage.Status)
	assert.Equal(t, "pass", verified.PerRecordMembership.Status)
	assert.NotEqual(t, mustDigest(t, plain), mustDigest(t, declared))
	delete(declared, "extensions")
	assert.Equal(t, mustDigest(t, plain), mustDigest(t, declared), "the extension is the only difference")
}

func TestAssembleBundleRejectsMalformedProducerKey(t *testing.T) {
	profile, key := profileFixture(t)
	store, log, root := checkpointedStore(t, key, profile.LogID)
	_, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root, ClosureDepth: 2, Payloads: "none", ProducerKey: ed25519.PublicKey{1, 2, 3}})
	assert.ErrorIs(t, err, ErrInput)
}

// selfCountersigned builds a bundle declaring countersignKey as the
// producer's (or declaring nothing), then countersigns it with that key: the
// shape of an operator countersigning its own bundle with a key that is not
// its ledger key.
func selfCountersigned(t *testing.T, declare bool) (map[string]interface{}, Profile, ed25519.PublicKey) {
	t.Helper()
	profile, key := profileFixture(t)
	store, log, root := checkpointedStore(t, key, profile.LogID)
	countersignPublic, countersignKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	options := BundleOptions{Root: root, ClosureDepth: 2, Payloads: "none"}
	if declare {
		options.ProducerKey = countersignPublic
	}
	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, options)
	require.NoError(t, err)
	entry := CountersignatureEntry{
		Type:      CountersignAPI,
		Signer:    CountersignSigner{ID: "countersign.example", KeyID: hex.EncodeToString(countersignPublic)},
		Over:      mustDigest(t, bundle),
		Statement: CountersignStatement{RecomputedAt: "2026-10-01T00:00:00Z", Scope: CountersignScope{LedgerID: profile.LogID}},
	}
	signCountersignEntry(t, countersignKey, &entry)
	require.NoError(t, AttachCountersignatures(bundle, []CountersignatureEntry{entry}))
	return bundle, profile, countersignPublic
}

func TestCountersignVerifyDeclaredProducerKeyIsNotIndependent(t *testing.T) {
	directory := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"countersigners":[]}`))
	}))
	defer directory.Close()
	client := &http.Client{Transport: directory.Client().Transport, Timeout: 5 * time.Second}

	// The verifying profile trusts only the ledger key; the bundle itself
	// declares the countersigning key as the producer's.
	bundle, profile, _ := selfCountersigned(t, true)
	trusted, err := ParseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	_, reports, summary, err := VerifyCountersignatures(t.Context(), client, directory.URL, bundle, trusted)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "not_independent", reports[0].State)
	assert.False(t, *reports[0].Independent)
	assert.Equal(t, "not_independent", summary)

	// Without the declaration the same signer is merely unlisted.
	bundle, profile, _ = selfCountersigned(t, false)
	trusted, err = ParseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	_, reports, _, err = VerifyCountersignatures(t.Context(), client, directory.URL, bundle, trusted)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "unresolved_signer", reports[0].State)
}

// A declaration in any other form than the viewer accepts declares nothing.
func TestDeclaredProducerKeyRequiresTheViewerForm(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	lower := hex.EncodeToString(public)
	for name, value := range map[string]interface{}{
		"upper case": strings.ToUpper(lower), "short": lower[:62], "number": json.Number("1"), "absent": nil,
	} {
		bundle := map[string]interface{}{"extensions": map[string]interface{}{"producer-key/v1": map[string]interface{}{"public_key": value}}}
		_, ok := DeclaredProducerKey(bundle)
		assert.False(t, ok, name)
	}
	key, ok := DeclaredProducerKey(map[string]interface{}{"extensions": map[string]interface{}{"producer-key/v1": map[string]interface{}{"public_key": lower}}})
	require.True(t, ok)
	assert.Equal(t, public, key)
}

// The SDK-storage path (sqlite) declares the profile's signing key too.
func TestBundleCommandDeclaresProducerKeyOnSQLite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Type = "sqlite"
	p.Connection.Database = filepath.Join(t.TempDir(), "store.db")
	require.NoError(t, SaveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	request, err := ParseRequest(requestFixture(t))
	require.NoError(t, err)
	target, err := OpenTarget(t.Context(), p, UsePublication)
	require.NoError(t, err)
	published, err := target.Publish(t.Context(), request, key)
	require.NoError(t, err)
	require.NoError(t, target.Close())
	_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)

	out, err := invoke(t, "", "bundle", "--profile", p.Name, "--root", published.CapsuleID)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(key.Public().(ed25519.PublicKey)), viewerProducerKey(t, []byte(out)))
	bundle, err := DecodeBundleJSON([]byte(out))
	require.NoError(t, err)
	verified := aacbundle.VerifyBundle(bundle)
	assert.Equal(t, "pass", verified.PerRecordMembership.Status)
}

// bundle --html writes the bundle as one self-contained page: the bundle
// embedded and capsule-viewer's page runtime, which checks it offline.
func TestBundleHTMLWritesASelfCheckingPage(t *testing.T) {
	// A sqlite profile: a jsonl profile writes no page (page_gate_test.go).
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Type = "sqlite"
	p.Connection.Database = filepath.Join(t.TempDir(), "store.db")
	require.NoError(t, SaveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	request, err := ParseRequest(requestFixture(t))
	require.NoError(t, err)
	target, err := OpenTarget(t.Context(), p, UsePublication)
	require.NoError(t, err)
	published, err := target.Publish(t.Context(), request, key)
	require.NoError(t, err)
	require.NoError(t, target.Close())
	_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)

	page := filepath.Join(t.TempDir(), "bundle.html")
	out, err := invoke(t, "", "bundle", "--profile", p.Name, "--root", published.CapsuleID, "--html", page)
	require.NoError(t, err)
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	html := string(raw)
	assert.Contains(t, html, string(pageruntime.IIFE), "the verifier is in the page")
	assert.NotRegexp(t, regexp.MustCompile(`(?i)<(script|link|img|iframe)[^>]+(src|href)=`), html, "nothing is fetched")
	embedded, err := json.Marshal(embeddedBundle(t, html))
	require.NoError(t, err)
	var stdout, inPage map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &stdout))
	require.NoError(t, json.Unmarshal(embedded, &inPage))
	assert.Equal(t, stdout["records"], inPage["records"], "the page holds the bundle the command wrote")
	v := aacbundle.VerifyBundle(embeddedBundle(t, html))
	assert.Equal(t, "pass", v.IntervalCoverage.Status, v.IntervalCoverage.Findings)
	assert.Equal(t, "pass", v.PerRecordMembership.Status, v.PerRecordMembership.Findings)

	// An existing file is never overwritten; a permalink and a deal take no --html.
	_, err = invoke(t, "", "bundle", "--profile", p.Name, "--root", published.CapsuleID, "--html", page)
	require.Error(t, err)
	_, err = invoke(t, "", "permalink", "--profile", p.Name, "--root", published.CapsuleID, "--html", filepath.Join(t.TempDir(), "x.html"))
	require.ErrorIs(t, err, ErrInput)
}

// embeddedBundle recovers the bundle the emitter embedded in the page.
func embeddedBundle(t *testing.T, html string) map[string]interface{} {
	t.Helper()
	const start = "window.__BUNDLE__ = "
	i := strings.Index(html, start)
	require.GreaterOrEqual(t, i, 0)
	rest := html[i+len(start):]
	j := strings.Index(rest, ";</script>")
	require.GreaterOrEqual(t, j, 0)
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(rest[:j])))
	require.NoError(t, err)
	b, ok := v.(map[string]interface{})
	require.True(t, ok)
	return b
}
