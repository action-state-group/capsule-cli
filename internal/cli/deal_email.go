package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// A deal receipt reaches the user by email: the one channel that reads the
// same on a phone, a messenger's mail view, a desktop and the web. capsulectl
// only writes the message; the agent host's own email tool sends it, so the
// receipt never passes through any service of this project. The message
// carries a short plain-text body, the same report as a static HTML body (no
// scripts, no remote content: mail apps run neither), the self-checking
// receipt page as an attachment, and the Evidence Bundle so anyone can check
// it with `capsulectl verify --bundle`.

// heldWitnessReceipt returns the checkpoint.witnesses entry for the witness
// receipt this device holds for the signed checkpoint statement, or nil when
// no witness is configured, none was received, or it does not verify under
// the profile's pinned witness key.
func (s *dealSession) heldWitnessReceipt(ctx context.Context, statement []byte) (map[string]interface{}, error) {
	service, err := serviceID(s.dp)
	if err != nil || service == "" {
		return nil, err
	}
	record, err := verifyCheckpoint(s.dp, statement)
	if err != nil {
		return nil, err
	}
	state, err := s.t.log.GetWitness(ctx, service, record.MMRSize)
	if err != nil || state.Receipt == nil || verifyWitness(s.dp, state) != nil {
		return nil, nil
	}
	entry := map[string]interface{}{
		"ts_url":      strings.TrimRight(s.dp.Checkpoint.Endpoint, "/"),
		"entry_hash":  state.Receipt.EntryHash,
		"receipt_b64": base64.StdEncoding.EncodeToString(state.Receipt.Bytes),
	}
	if state.Receipt.LeafIndex != nil {
		entry["leaf_index"] = integer(uint64(*state.Receipt.LeafIndex))
	}
	if state.Receipt.TreeSize != nil {
		entry["tree_size"] = integer(uint64(*state.Receipt.TreeSize))
	}
	return entry, nil
}

// dealAssurance names the rung the report stands on, from the bundle itself:
// "witnessed" when it carries a witness receipt (added only after it
// re-verified under the pinned witness key), otherwise sealed by the agent's
// own device and nothing more is claimed.
func dealAssurance(b map[string]interface{}) map[string]any {
	cp, _ := b["checkpoint"].(map[string]interface{})
	witnesses, _ := cp["witnesses"].([]interface{})
	if len(witnesses) == 0 {
		return map[string]any{"rung": "sealed", "text": "Sealed by my agent: tamper-evident, not non-repudiation. The key that sealed it is on the agent's own device."}
	}
	entry, _ := witnesses[0].(map[string]interface{})
	host := fmt.Sprint(entry["ts_url"])
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	return map[string]any{"rung": "witnessed", "witness": host, "text": fmt.Sprintf(
		"Witnessed: %s, an independent log, signed a receipt for the checkpoint covering these steps. The receipt is in the attached bundle; check it with capsulectl verify --bundle bundle.json --witness-directory DIRECTORY.json, using a witness directory you trust.", host)}
}

type dealEmailView struct {
	Demo        bool
	Asked       string
	Outcome     string
	Assurance   string
	Did         []dealReportItem
	Anomalies   []dealReportItem
	Steps       int
	VerifyLine  string
	NotClaimed  []string
	CheckedNote string
}

var dealEmailNotClaimed = []string{
	"Tamper-evident, not non-repudiation: it shows the record was not changed after it was made, not who made it.",
	"It records what the agent reported doing; it does not prove what the merchant charged or shipped.",
	"Calling the deal check is advisory: steps the agent never sealed are not in it.",
}

const dealEmailVerify = "capsulectl verify --bundle bundle.json"

var dealEmailHTML = template.Must(template.New("email").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Deal receipt</title></head>
<body style="margin:0;padding:16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#1a1a1a;background:#ffffff;">
<div style="max-width:640px;margin:0 auto;">
<h1 style="font-size:20px;margin:0 0 8px;">Deal receipt{{if .Demo}} <span style="font-size:13px;border:1px solid #b45309;color:#b45309;padding:0 6px;border-radius:4px;">DEMO</span>{{end}}</h1>
<p style="margin:0 0 16px;color:#444;">{{.Assurance}}</p>
<h2 style="font-size:16px;margin:16px 0 4px;">What you asked</h2>
<p style="margin:0;">&ldquo;{{.Asked}}&rdquo;</p>
<h2 style="font-size:16px;margin:16px 0 4px;">What the agent did</h2>
<ul style="margin:0;padding-left:20px;">{{range .Did}}<li>{{.Text}}</li>{{else}}<li>Nothing yet.</li>{{end}}</ul>
<p style="margin:4px 0 0;color:#444;">Outcome: {{.Outcome}} &middot; {{.Steps}} sealed steps</p>
<h2 style="font-size:16px;margin:16px 0 4px;">Anomalies</h2>
<ul style="margin:0;padding-left:20px;">{{range .Anomalies}}<li>{{if .Side}}{{.Side}} side: {{end}}{{.Text}}</li>{{else}}<li>None found.</li>{{end}}</ul>
<h2 style="font-size:16px;margin:16px 0 4px;">Check it yourself</h2>
<p style="margin:0;">{{.CheckedNote}}</p>
<pre style="margin:8px 0 0;padding:8px;background:#f4f4f0;border-radius:4px;white-space:pre-wrap;word-break:break-all;">{{.VerifyLine}}</pre>
<h2 style="font-size:16px;margin:16px 0 4px;">What this does not claim</h2>
<ul style="margin:0;padding-left:20px;color:#444;">{{range .NotClaimed}}<li>{{.}}</li>{{end}}</ul>
</div></body></html>
`))

// dealEmail builds the receipt message. It has no From or To: the agent
// host's email tool addresses and sends it.
func dealEmail(view dealEmailView, page, bundle []byte, at time.Time) (eml []byte, subject, text, htmlBody string, err error) {
	view.VerifyLine = dealEmailVerify
	view.NotClaimed = dealEmailNotClaimed
	view.CheckedNote = "This copy cannot check itself: mail apps do not run scripts. Open the attached receipt.html in a browser, where it checks every sealed step offline, or save bundle.json and run:"
	asked := view.Asked
	if r := []rune(asked); len(r) > 60 {
		asked = string(r[:59]) + "…"
	}
	subject = "Deal receipt: " + asked
	if view.Demo {
		subject = "DEMO · " + subject
	}
	var tb strings.Builder
	if view.Demo {
		tb.WriteString("DEMO\n\n")
	}
	fmt.Fprintf(&tb, "What you asked: \"%s\"\n\n%s\n\nWhat the agent did:\n", view.Asked, view.Assurance)
	if len(view.Did) == 0 {
		tb.WriteString("- Nothing yet.\n")
	}
	for _, d := range view.Did {
		fmt.Fprintf(&tb, "- %s\n", d.Text)
	}
	fmt.Fprintf(&tb, "Outcome: %s · %d sealed steps\n\nAnomalies:\n", view.Outcome, view.Steps)
	if len(view.Anomalies) == 0 {
		tb.WriteString("- None found.\n")
	}
	for _, a := range view.Anomalies {
		side := ""
		if a.Side != "" {
			side = a.Side + " side: "
		}
		fmt.Fprintf(&tb, "- %s%s\n", side, a.Text)
	}
	fmt.Fprintf(&tb, "\nCheck it yourself: %s\n  %s\n\nWhat this does not claim:\n", view.CheckedNote, view.VerifyLine)
	for _, n := range view.NotClaimed {
		fmt.Fprintf(&tb, "- %s\n", n)
	}
	text = tb.String()
	var hb bytes.Buffer
	if err = dealEmailHTML.Execute(&hb, view); err != nil {
		return nil, "", "", "", err
	}
	htmlBody = hb.String()

	var msg bytes.Buffer
	mixed := multipart.NewWriter(&msg)
	fmt.Fprintf(&msg, "MIME-Version: 1.0\r\nDate: %s\r\nSubject: %s\r\nContent-Type: multipart/mixed; boundary=%s\r\n\r\n",
		at.UTC().Format(time.RFC1123Z), mime.QEncoding.Encode("utf-8", subject), mixed.Boundary())
	var altBody bytes.Buffer
	alt := multipart.NewWriter(&altBody)
	for _, part := range []struct{ ctype, body string }{{"text/plain; charset=utf-8", text}, {"text/html; charset=utf-8", htmlBody}} {
		w, err := alt.CreatePart(textproto.MIMEHeader{"Content-Type": {part.ctype}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, "", "", "", err
		}
		qp := quotedprintable.NewWriter(w)
		if _, err = qp.Write([]byte(part.body)); err != nil {
			return nil, "", "", "", err
		}
		if err = qp.Close(); err != nil {
			return nil, "", "", "", err
		}
	}
	if err = alt.Close(); err != nil {
		return nil, "", "", "", err
	}
	w, err := mixed.CreatePart(textproto.MIMEHeader{"Content-Type": {"multipart/alternative; boundary=" + alt.Boundary()}})
	if err != nil {
		return nil, "", "", "", err
	}
	if _, err = w.Write(altBody.Bytes()); err != nil {
		return nil, "", "", "", err
	}
	for _, a := range []struct {
		name, ctype string
		body        []byte
	}{{"receipt.html", "text/html; charset=utf-8", page}, {"bundle.json", "application/json", bundle}} {
		w, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {a.ctype + "; name=\"" + a.name + "\""},
			"Content-Disposition":       {"attachment; filename=\"" + a.name + "\""},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return nil, "", "", "", err
		}
		enc := base64.StdEncoding.EncodeToString(a.body)
		for len(enc) > 76 {
			if _, err = w.Write([]byte(enc[:76] + "\r\n")); err != nil {
				return nil, "", "", "", err
			}
			enc = enc[76:]
		}
		if _, err = w.Write([]byte(enc + "\r\n")); err != nil {
			return nil, "", "", "", err
		}
	}
	if err = mixed.Close(); err != nil {
		return nil, "", "", "", err
	}
	return msg.Bytes(), subject, text, htmlBody, nil
}
