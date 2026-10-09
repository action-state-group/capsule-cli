package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referenceKnownUnsigned is the repository's reference copy of the tags made
// before tag signing began.
var referenceKnownUnsigned = filepath.Join("..", "..", "release", "known-unsigned.txt")

// beforeSigning is every tag made before tag signing began: v0.1.0-rc1 to
// v0.1.0-rc14.
func beforeSigning() []string {
	var tags []string
	for i := 1; i <= 14; i++ {
		tags = append(tags, fmt.Sprintf("v0.1.0-rc%d", i))
	}
	return tags
}

// The reference list names exactly rc1 to rc14 and is frozen: its SHA-256 is
// the one the transparency document tells the monitor's operator to check
// their copy against, so the file cannot change without the document.
func TestTheReferenceKnownUnsignedListIsFrozenAtRc14(t *testing.T) {
	tags, err := readKnownUnsigned(referenceKnownUnsigned)
	require.NoError(t, err)
	assert.Equal(t, beforeSigning(), tags)
	raw := mustRead(t, referenceKnownUnsigned)
	assert.Contains(t, string(raw), "# Tags made before tag signing began (rc15 is the first signed tag).")

	sum := sha256.Sum256(raw)
	doc := string(mustRead(t, filepath.Join("..", "..", "docs", "RELEASE-TRANSPARENCY.md")))
	stated := regexp.MustCompile("(?s)release/known-unsigned.txt.*?```text\n([0-9a-f]{64})\n```").FindStringSubmatch(doc)
	require.Len(t, stated, 2, "the document states the reference copy's SHA-256")
	assert.Equal(t, hex.EncodeToString(sum[:]), stated[1], "the document's SHA-256 is the file's")
}

// A monitor given its copy of the reference list accepts rc1 to rc14 as made
// before signing and a signed rc15 as intended, with no alarm. A new tag
// pushed unsigned is not in the list, so it is reported unsigned, never
// silently accepted, whatever its name.
func TestReleaseWatchWithTheReferenceList(t *testing.T) {
	// The operator's copy, taken once and checked.
	copied := filepath.Join(t.TempDir(), "known-unsigned")
	require.NoError(t, os.WriteFile(copied, mustRead(t, referenceKnownUnsigned), 0o600))

	w := newReleaseWorld(t)
	for _, tag := range beforeSigning() {
		w.addTag(t, tag, nil)
		w.addRelease(t, tag, false)
	}
	w.addTag(t, "v0.1.0-rc15", &w.signer)
	w.addRelease(t, "v0.1.0-rc15", true)
	result, err := w.watch(t, "--known-unsigned-file", copied)
	require.NoError(t, err, alarmsOf(result))
	assert.Equal(t, "ok", result["state"])
	assert.Empty(t, result["alarms"], "no tag made before signing is reported unsigned")

	for _, tag := range []string{"v0.1.0-rc16", "v0.1.0-rc1-scratch"} {
		t.Run(tag, func(t *testing.T) {
			w.addTag(t, tag, nil)
			result, err := w.watch(t, "--known-unsigned-file", copied)
			require.ErrorIs(t, err, ErrAlarm)
			assert.Contains(t, alarmsOf(result), "unintended tag "+tag+": unsigned")
			w.gh.tags = w.gh.tags[:len(w.gh.tags)-1]
		})
	}
}
