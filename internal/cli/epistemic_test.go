package cli

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonicalEpistemicTypes is the closed epistemic_type value set, transcribed
// from agent-action-capsule's schemas/vendor/epistemic-types.json (itself a
// transcription of draft-mih-agent-evidence-layer-00 section "Epistemic
// Type"). The tokens are lowercase ASCII snake_case in spec text, schemas,
// vectors and code.
var canonicalEpistemicTypes = []string{
	"observed_event",
	"system_of_record_fact",
	"producer_claim",
	"human_report",
	"semantic_judgment",
	"derived_metric",
	"adjudication",
	"obligation_reference",
}

var lowerSnake = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// The evidencebook constants capsulectl writes with are the canonical
// lowercase tokens, in registry order.
func TestEpistemicTypeConstantsAreTheCanonicalLowercaseSet(t *testing.T) {
	var got []string
	for _, et := range evidencebook.EpistemicTypes() {
		got = append(got, string(et))
		assert.Regexp(t, lowerSnake, string(et))
	}
	assert.Equal(t, canonicalEpistemicTypes, got)
}

// Every record capsulectl puts in a book (backfill, migration, publish, and
// the Close, disclosure and acknowledgement records under them) carries a
// lowercase epistemic_type from the canonical set.
func TestEveryEmittedEpistemicTypeIsLowercaseAndKnown(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 2, 1, false)
	_, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	publishFile(t, "a", sealRequestFile(t, "emitted-1"))
	set(day1)
	dir := t.TempDir()
	_, err = runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b",
		"--capsule-out", filepath.Join(dir, "close.json"), "--bundle-out", filepath.Join(dir, "close-bundle.json"))
	require.NoError(t, err)

	p, err = loadProfile("a")
	require.NoError(t, err)
	opened, err := openBook(t.Context(), p, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	records, err := opened.book.Query(t.Context(), evidencebook.Filter{})
	require.NoError(t, err)

	recordTypes := map[string]bool{}
	for _, r := range records {
		recordTypes[r.Header.RecordType] = true
		et := string(r.Header.EpistemicType)
		assert.Regexp(t, lowerSnake, et, "record %d (%s)", r.Seq, r.Header.RecordType)
		assert.Contains(t, canonicalEpistemicTypes, et, "record %d (%s)", r.Seq, r.Header.RecordType)
	}
	for _, want := range []string{recordTypeBackfilled, recordTypeMigration, recordTypePublished, string(evidencebook.RecordTypeClose)} {
		assert.True(t, recordTypes[want], "the scenario emits a %s record", want)
	}
}
