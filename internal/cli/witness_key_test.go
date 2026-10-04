package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const witnessKeyFix = "capsulectl profile update --profile test --checkpoint-public-key <64-hex Ed25519 key>"

// TestServiceIDNamesMissingWitnessKey: an endpoint without its public key
// fails before any network call, with an error that names the field and the
// fix rather than the generic profile-configuration text.
func TestServiceIDNamesMissingWitnessKey(t *testing.T) {
	p, _ := profileFixture(t)
	p.Checkpoint.Endpoint = "https://witness.example/checkpoints"
	_, err := serviceID(p)
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, ErrInput.Error()+": checkpoint.public_key is required when checkpoint.endpoint is set: "+witnessKeyFix, SafeError(err))
	assert.Equal(t, 2, ExitCode(err))

	p.Checkpoint.PublicKey = "not-hex"
	_, err = serviceID(p)
	assert.Contains(t, SafeError(err), "checkpoint.public_key must be the witness's Ed25519 public key as 64 hex characters: "+witnessKeyFix)

	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	id, err := serviceID(p)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	p.Checkpoint.Endpoint = "http://witness.example"
	_, err = serviceID(p)
	assert.Contains(t, SafeError(err), "checkpoint.endpoint must be an HTTPS URL")
}

// TestProfileUpdateEndpointNeedsPublicKey: `profile update` never writes a
// profile whose witness endpoint has no key; the endpoint and its key are set
// in one call, and a key alone repairs a profile saved without one.
func TestProfileUpdateEndpointNeedsPublicKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	key := p.TrustedKeys[0]

	_, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--checkpoint-endpoint", "https://witness.example")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--checkpoint-endpoint needs --checkpoint-public-key <64-hex Ed25519 key> in the same call")
	stored, err := loadProfile(p.Name)
	require.NoError(t, err)
	assert.Empty(t, stored.Checkpoint.Endpoint, "nothing was written")

	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--checkpoint-endpoint", "https://witness.example", "--checkpoint-public-key", "39bb654c")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "64 hex characters")

	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--checkpoint-endpoint", "https://witness.example", "--checkpoint-public-key", key)
	require.NoError(t, err)
	stored, err = loadProfile(p.Name)
	require.NoError(t, err)
	assert.Equal(t, "https://witness.example", stored.Checkpoint.Endpoint)
	assert.Equal(t, key, stored.Checkpoint.PublicKey)

	// A profile already saved with an endpoint and no key (by an older
	// capsulectl) still loads, and a key alone repairs it.
	stored.Checkpoint.PublicKey = ""
	require.NoError(t, saveProfile(stored, true))
	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--clock-tolerance", "2m")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "checkpoint.public_key is required when checkpoint.endpoint is set")
	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--checkpoint-public-key", key)
	require.NoError(t, err)

	// profile create refuses the same shape.
	_, err = invoke(t, "", "profile", "create", "--name", "other", "--type", "jsonl", "--jsonl-path", t.TempDir(), "--log-id", "other", "--checkpoint-endpoint", "https://witness.example")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "checkpoint.public_key is required when checkpoint.endpoint is set")
}
