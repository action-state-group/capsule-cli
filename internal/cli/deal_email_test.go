package cli

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emailParts reads a .eml as a mail app would: the plain and HTML bodies and
// each attachment by file name.
func emailParts(t *testing.T, raw []byte) (*mail.Message, map[string]string, map[string][]byte) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	media, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/mixed", media)
	bodies, files := map[string]string{}, map[string][]byte{}
	mixed := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := mixed.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		media, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		require.NoError(t, err)
		if media == "multipart/alternative" {
			alt := multipart.NewReader(part, params["boundary"])
			for {
				p, err := alt.NextRawPart()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				ctype, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
				body, err := io.ReadAll(quotedprintable.NewReader(p))
				require.NoError(t, err)
				bodies[ctype] = string(body)
			}
			continue
		}
		_, disp, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		require.NoError(t, err)
		body, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		require.NoError(t, err)
		files[disp["filename"]] = body
	}
	return msg, bodies, files
}

func TestDealReceiptEmailIsReadableWithoutADownloadAndCheckable(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", filepath.Join(retailDemo, "act-pay.json"))
	dir := t.TempDir()
	eml, bundlePath := filepath.Join(dir, "receipt.eml"), filepath.Join(dir, "bundle.json")
	report := dealRun(t, "report", "--deal", dealID, "--email", eml, "--bundle", bundlePath)
	assert.Equal(t, "self_attested", report["assurance"].(map[string]any)["rung"], "no witness configured: sealed by my agent, nothing more")

	raw, err := os.ReadFile(eml)
	require.NoError(t, err)
	msg, bodies, files := emailParts(t, raw)
	assert.Empty(t, msg.Header.Get("From"), "the agent host's email tool addresses and sends it")
	assert.Empty(t, msg.Header.Get("To"))
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	require.NoError(t, err)
	assert.Equal(t, "DEMO · Deal receipt: Order the cat sticker in my cart at Example Stickers", subject)

	// Readable in the message itself, on any mail app, with nothing to open.
	text, page := bodies["text/plain"], bodies["text/html"]
	for _, body := range []string{text, page} {
		assert.Contains(t, body, "Order the cat sticker in my cart at Example Stickers")
		assert.Contains(t, body, "Sealed by my agent")
		assert.Contains(t, body, "capsulectl verify --bundle bundle.json")
		assert.Contains(t, body, "This copy cannot check itself")
		assert.NotContains(t, strings.ToLower(body), "verified", "never claim a check the message cannot make")
	}
	assert.Contains(t, page, "Anomalies")
	for _, banned := range []string{"<script", "http://", "https://", "src=", "<link", "<img"} {
		assert.NotContains(t, strings.ToLower(page), banned, "the HTML body is static and loads nothing")
	}
	assert.Equal(t, strings.ReplaceAll(text, "\r\n", "\n"), report["email"].(map[string]any)["text"], "mail line endings are CRLF")

	// The attachments: the self-checking page and the bundle.
	require.Contains(t, files, "receipt.html")
	assert.Regexp(t, `<title>Deal report: “[^<]+”</title>`, string(files["receipt.html"]), "the user's own copy is titled with the words they asked")
	written, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	assert.Equal(t, written, files["bundle.json"])
	out, err := invoke(t, "", "verify", "--bundle", bundlePath)
	require.NoError(t, err, out)

	// Like --html, the files are new: an existing one is never overwritten.
	_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", dealID, "--email", eml)
	require.Error(t, err)
}
