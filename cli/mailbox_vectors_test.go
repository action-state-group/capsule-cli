package cli

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The AAC nostr-mailbox-v0 vectors (agent-action-capsule
// vectors/adapters/nostr-mailbox, byte copy in testdata/nostr-mailbox): every
// delivered event is classified as the vectors name it, which fixes checks 1
// to 7, a reply by content, the limit and a delivery counted once. The
// classification does not depend on the carriage code that published them.
type mailboxVectors struct {
	Keys struct {
		Responder string `json:"responder"`
	} `json:"evidence_test_keys"`
	Cases []struct {
		ID     string `json:"id"`
		Inputs struct {
			Request struct {
				MediaType  string `json:"media_type"`
				OctetsHex  string `json:"octets_hex"`
				OctetsText string `json:"octets_text"`
			} `json:"request"`
			Mailbox struct {
				Key          string       `json:"mailbox_key"`
				Route        string       `json:"route"`
				RequestEvent nostrEvent   `json:"request_event"`
				Events       []nostrEvent `json:"events"`
			} `json:"mailbox"`
		} `json:"inputs"`
		Expected struct {
			RequestDigest  string   `json:"request_digest"`
			RequestEventID string   `json:"request_event_id"`
			Classification []string `json:"classification"`
		} `json:"expected"`
	} `json:"cases"`
}

func loadMailboxVectors(t *testing.T) mailboxVectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "nostr-mailbox", "vectors.json"))
	require.NoError(t, err)
	var v mailboxVectors
	require.NoError(t, json.Unmarshal(raw, &v))
	require.NotEmpty(t, v.Cases)
	return v
}

func TestMailboxVectorsClassifyEveryDeliveredEvent(t *testing.T) {
	v := loadMailboxVectors(t)
	for _, c := range v.Cases {
		t.Run(c.ID, func(t *testing.T) {
			in := c.Inputs
			octets := []byte(in.Request.OctetsText)
			if in.Request.OctetsHex != "" {
				var err error
				octets, err = hex.DecodeString(in.Request.OctetsHex)
				require.NoError(t, err)
			}
			digest := requestDigest(octets)
			require.Equal(t, c.Expected.RequestDigest, digest, "the digest is over the decoded octets (NMB §5.1)")
			proves := func(b []byte) bool {
				return answerTo(b, in.Request.MediaType, v.Keys.Responder, digest) == nil
			}
			got := classifyReplies(in.Mailbox.Events, c.Expected.RequestEventID, in.Request.MediaType, in.Mailbox.Key, proves, 2)
			var classes []string
			for _, g := range got {
				classes = append(classes, g.Class)
			}
			assert.Equal(t, orEmpty(c.Expected.Classification), orEmpty(classes))
			route, err := nprofileURI(in.Mailbox.Key, []string{"wss://relay.example"})
			require.NoError(t, err)
			assert.Equal(t, in.Mailbox.Route, route, "the route an absence record keeps")
			// The request event is the one the vectors publish: it verifies, and
			// our builder makes the same tags for the same input.
			require.NoError(t, in.Mailbox.RequestEvent.verify())
			assert.Equal(t, "p", in.Mailbox.RequestEvent.Tags[0][0])
		})
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
