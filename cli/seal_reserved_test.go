package cli

import (
	"testing"

	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
