package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type heldDealStore bool

func (h heldDealStore) HoldsDealStore() bool { return bool(h) }

// OpenTarget never opens a deal profile for writing; OpenDealTarget does,
// only for a writer that holds the deal store's lock. Reads open either way.
func TestOpenDealTarget(t *testing.T) {
	ctx := t.Context()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	p := Profile{Name: "deal", Type: "sqlite", Namespace: "deal", LogID: "deal-cadence/0123456789abcdef", TrustedKeys: []string{hex.EncodeToString(public)}}
	p.Connection.Database = filepath.Join(t.TempDir(), "deal.db")
	require.True(t, p.IsShelf())
	for _, use := range []TargetUse{UseCLL, UsePublication, UseInitialization} {
		_, err = OpenTarget(ctx, p, use)
		assert.ErrorIs(t, err, ErrDealProfileWrite, "OpenTarget use %d", use)
		_, err = OpenDealTarget(ctx, p, use, nil)
		assert.ErrorIs(t, err, ErrDealProfileWrite, "no writer, use %d", use)
		_, err = OpenDealTarget(ctx, p, use, heldDealStore(false))
		assert.ErrorIs(t, err, ErrDealProfileWrite, "a released writer, use %d", use)
	}
	tg, err := OpenDealTarget(ctx, p, UseInitialization, heldDealStore(true))
	require.NoError(t, err)
	require.NoError(t, tg.Close())
	tg, err = OpenTarget(ctx, p, UseCLLRead)
	require.NoError(t, err, "a read opens without the writer")
	require.NoError(t, tg.Close())

	// A side log of the store: the deal plugin clears the namespace to open
	// it without the artifact store.
	side := p
	side.Namespace, side.LogID = "", "deal/deal-0123456789abcdef/requests"
	tg, err = OpenDealTarget(ctx, side, UseInitialization, heldDealStore(true))
	require.NoError(t, err)
	require.NoError(t, tg.Close())
	_, err = OpenDealTarget(ctx, side, UseInitialization, nil)
	assert.ErrorIs(t, err, ErrDealProfileWrite)
}

// opening.* names an evidence book's field openings: no request may name its
// own artifact so, on any profile, and other names seal as before.
func TestSealReservesOpeningNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, key := profileFixture(t)
	request, err := ParseRequest(requestFixture(t))
	require.NoError(t, err)
	request.Artifacts = []artifact.Artifact{{Name: "opening.receipt", State: artifact.Present, Content: []byte("x")}}
	_, err = Seal(request, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening.receipt is reserved")
	request.Artifacts = []artifact.Artifact{{Name: "receipt", State: artifact.Present, Content: []byte("x")}}
	_, err = Seal(request, key)
	require.NoError(t, err)
}
