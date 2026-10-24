package book

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewerPublicKey is the AAC viewer's producer-key/v1 acceptance rule.
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

// The bundle verb on a jsonl profile declares the profile's signing key by
// default, and --producer-key overrides it; the book digests the extension
// into its disclosure record.
func TestBundleCommandDeclaresProducerKeyOnTheBook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, cli.SaveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	request, err := cli.ParseRequest(requestFixture(t))
	require.NoError(t, err)
	target, err := cli.OpenTarget(t.Context(), p, cli.UsePublication)
	require.NoError(t, err)
	published, err := target.Publish(t.Context(), request, key)
	require.NoError(t, err)
	require.NoError(t, target.Close())

	out, err := invoke(t, "", "bundle", "--profile", p.Name, "--root", published.CapsuleID)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(key.Public().(ed25519.PublicKey)), viewerProducerKey(t, []byte(out)))
	verified, err := evidencebook.VerifyBundle([]byte(strings.TrimSpace(out)))
	require.NoError(t, err)
	assert.Contains(t, verified.Extensions, "producer-key/v1")

	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	out, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", published.CapsuleID, "--producer-key", strings.ToUpper(hex.EncodeToString(other)))
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(other), viewerProducerKey(t, []byte(out)), "declared in the viewer's lowercase form")

	_, err = invoke(t, "", "bundle", "--profile", p.Name, "--root", published.CapsuleID, "--producer-key", "not-a-key")
	assert.ErrorIs(t, err, cli.ErrInput)
}
