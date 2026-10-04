package cli

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// A version-marked, compressed Evidence Bundle URL fragment, kept for a
// possible future viewer that reads it. Nothing in capsulectl uses it today:
// a deal is not shared as a link, and `permalink` mints the plain fragment.
// It is kept, with its tests, so the codec and its measurements are not lost
// if a viewer adopts a compressed fragment later.
//
// Shape: "z1." + unpadded base64url(deflate-raw(JCS bytes of the bundle)).
// The "." never appears in a plain fragment (unpadded base64url), so a decoder
// tells the two apart without guessing, and every existing fragment keeps
// decoding as before. "z1" names the codec: deflate-raw (RFC 1951), the
// format a browser's DecompressionStream("deflate-raw") reads.

const (
	fragmentCodecZ1 = "z1."
	// fragmentMaxInflated caps what a z1 fragment may inflate to, so a small
	// fragment cannot expand without bound (a compression bomb).
	fragmentMaxInflated = 1 << 20
)

var fragmentB64 = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)

// encodeFragmentZ1 deflates the bundle's JCS bytes and marks the codec.
func encodeFragmentZ1(value interface{}) (string, error) {
	jcs, err := canonical.JCS(value)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err = w.Write(jcs); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	return fragmentCodecZ1 + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// decodeFragmentAny decodes a plain fragment as before, a z1 fragment by
// inflating it under the cap, and refuses any other codec mark.
func decodeFragmentAny(fragment string) (interface{}, error) {
	mark, body, marked := strings.Cut(fragment, ".")
	if !marked {
		return aacbundle.DecodeFragment(fragment)
	}
	if mark+"." != fragmentCodecZ1 {
		return nil, errors.New("unsupported fragment codec: " + mark)
	}
	if !fragmentB64.MatchString(body) {
		return nil, errors.New("fragment must be unpadded base64url after the codec mark")
	}
	compressed, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, err
	}
	r := flate.NewReader(bytes.NewReader(compressed))
	defer r.Close()
	inflated, err := io.ReadAll(io.LimitReader(r, fragmentMaxInflated+1))
	if err != nil {
		return nil, err
	}
	if len(inflated) > fragmentMaxInflated {
		return nil, errors.New("fragment inflates past the size cap")
	}
	return aacbundle.DecodeFragment(base64.RawURLEncoding.EncodeToString(inflated))
}
