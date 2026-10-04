package cli

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The z1 codec, on a real deal bundle: it round-trips, a plain fragment still
// decodes, an unknown mark is refused, a compression bomb is refused at the
// cap, and a multi-step bundle compresses to well under half.
func TestFragmentCodecZ1(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", dealID)
	require.NoError(t, err)
	value, err := decodeBundleJSON([]byte(out))
	require.NoError(t, err)
	plain, err := aacbundle.EncodeFragment(value)
	require.NoError(t, err)

	z1, err := encodeFragmentZ1(value)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(z1, "z1."))
	back, err := decodeFragmentAny(z1)
	require.NoError(t, err)
	assert.Equal(t, any(value), back, "z1 round-trips")
	old, err := decodeFragmentAny(plain)
	require.NoError(t, err)
	assert.Equal(t, any(value), old, "a plain fragment still decodes")
	assert.Less(t, len(z1)*2, len(plain), "z1 at least halves the fragment")

	_, err = decodeFragmentAny("z2." + z1[3:])
	assert.ErrorContains(t, err, "unsupported fragment codec")
	_, err = decodeFragmentAny("z1." + z1[3:] + "=")
	assert.ErrorContains(t, err, "unpadded base64url")
	// A compression bomb is refused at the cap.
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	require.NoError(t, err)
	_, err = w.Write(bytes.Repeat([]byte{' '}, fragmentMaxInflated+10))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	_, err = decodeFragmentAny("z1." + base64.RawURLEncoding.EncodeToString(buf.Bytes()))
	assert.ErrorContains(t, err, "size cap")
}
