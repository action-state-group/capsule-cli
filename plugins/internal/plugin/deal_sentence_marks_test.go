package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentenceMarksSHA256 pins testdata/sentence-marks.txt, the same bytes as
// capsule-engine's tests/fixtures/sentence-marks.txt.
const sentenceMarksSHA256 = "8869f15bbf6896160f67e2d6f19a0da449a3f33d1e12d4cb71efb77c7877b9aa"

func fixtureSentenceMarks(t *testing.T) []rune {
	t.Helper()
	raw, err := os.ReadFile("testdata/sentence-marks.txt")
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, sentenceMarksSHA256, hex.EncodeToString(sum[:]))
	var marks []rune
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "U+") {
			continue
		}
		n, err := strconv.ParseUint(line[2:], 16, 32)
		require.NoError(t, err, line)
		marks = append(marks, rune(n))
	}
	return marks
}

// The marks are the shared fixture, code point for code point, in order.
func TestAttributedSentenceMarksAreTheSharedFixture(t *testing.T) {
	want := fixtureSentenceMarks(t)
	assert.Len(t, want, 181)
	assert.Equal(t, want, []rune(attributedSentenceMarks))
}

// Every rune Go's Unicode tables call a sentence terminator is in the frozen
// list: when Go moves to a Unicode version with a new one, this fails, and
// the list (and the engine's) must be reconsidered, never silently left short.
func TestEverySentenceTerminatorGoKnowsIsAMark(t *testing.T) {
	marks := map[rune]bool{}
	for _, r := range attributedSentenceMarks {
		marks[r] = true
	}
	var missing []string
	for _, r16 := range unicode.Sentence_Terminal.R16 {
		for r := rune(r16.Lo); r <= rune(r16.Hi); r += rune(r16.Stride) {
			if !marks[r] {
				missing = append(missing, fmt.Sprintf("U+%04X", r))
			}
		}
	}
	for _, r32 := range unicode.Sentence_Terminal.R32 {
		for r := rune(r32.Lo); r <= rune(r32.Hi); r += rune(r32.Stride) {
			if !marks[r] {
				missing = append(missing, fmt.Sprintf("U+%04X", r))
			}
		}
	}
	assert.Empty(t, missing, "Unicode %s", unicode.Version)
}

// A sentence terminator in any script breaks the template.
func TestASentenceTerminalInAnyScriptBreaksTheTemplate(t *testing.T) {
	for _, mark := range []rune{0xFE52, 0xFE56, 0xFE57, 0x0589, 0x1362, 0x104B, 0x17D4, 0x0702, 0x1803, 0x2E2E,
		0xA4FF, 0x07F9, 0x11047, 0x1DA88} {
		text := "The seller says it's authentic" + string(mark) + "I checked it (unverified)"
		assert.False(t, attributedWords(text), "U+%04X", mark)
	}
}
