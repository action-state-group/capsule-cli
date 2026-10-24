package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runShared runs cmd under a minimal root that selects a profile, and
// returns what run received or the refusal.
func runShared(t *testing.T, cmd *cobra.Command, args ...string) error {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := invoke(t, "", "profile", "create", "--name", "p", "--type", "sqlite", "--sqlite-path", filepath.Join(t.TempDir(), "s.db"), "--log-id", "l", "--operator", "o")
	require.NoError(t, err)
	root := NewCommandWith(cmd)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{cmd.Name(), "--profile", "p"}, args...))
	return root.ExecuteContext(t.Context())
}

func TestEvidenceRequestCommandChecks(t *testing.T) {
	var got EvidenceRequestInput
	ran := 0
	cmd := func() *cobra.Command {
		return EvidenceRequestCommand(func(_ *cobra.Command, _ Profile, in EvidenceRequestInput) error {
			got, ran = in, ran+1
			return nil
		})
	}
	file := func(body string) string {
		path := filepath.Join(t.TempDir(), "f.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	signer, _, _ := ed25519.GenerateKey(rand.Reader)
	checkpoint, _, _ := ed25519.GenerateKey(rand.Reader)
	upper := strings.ToUpper(hex.EncodeToString(signer))

	for name, args := range map[string][]string{
		"no mode":                  {},
		"two modes":                {"--request", file("{}"), "--absent-until", "2026-10-24T00:00:00Z"},
		"ask with --for":           {"--request", file("{}"), "--for", "r1"},
		"answer without --for":     {"--response", file("{}")},
		"answer without keys":      {"--response", file("{}"), "--for", "r1"},
		"bad absent-until":         {"--absent-until", "yesterday", "--for", "r1"},
		"witnesses with no answer": {"--absent-until", "2026-10-24T00:00:00Z", "--for", "r1", "--witness-directory", file("{}")},
	} {
		err := runShared(t, cmd(), args...)
		require.ErrorIs(t, err, ErrInput, name)
	}
	assert.Zero(t, ran, "a refused input never reaches the plugin")

	require.NoError(t, runShared(t, cmd(), "--response", file(`{"a":1}`), "--for", "r1", "--responder-key", upper, "--responder-checkpoint-key", hex.EncodeToString(checkpoint), "--witness-directory", file(`{"w":1}`)))
	assert.Equal(t, `{"a":1}`, string(got.Response))
	assert.Equal(t, "r1", got.For)
	assert.Equal(t, hex.EncodeToString(signer), got.ResponderSigner, "keys arrive as lowercase hex")
	assert.Equal(t, `{"w":1}`, string(got.WitnessDirectory))

	require.NoError(t, runShared(t, cmd(), "--absent-until", "2026-10-24T00:00:00Z", "--for", "r1", "--route", "email"))
	assert.Equal(t, "2026-10-24T00:00:00Z", got.WindowEnd.Format("2006-01-02T15:04:05Z07:00"))
	assert.Equal(t, "email", got.Route)
	assert.Nil(t, got.Request)
}

func TestEvidenceRespondCommandNeedsOutput(t *testing.T) {
	cmd := EvidenceRespondCommand(func(*cobra.Command, Profile, EvidenceRespondInput) error {
		t.Fatal("run must not be reached")
		return nil
	})
	require.ErrorIs(t, runShared(t, cmd), ErrInput)
}
