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

// dealScopeLine is printed on the face of every receipt, report page and
// email: a receipt is about one deal, never about the agent's whole activity.
const dealScopeLine = "This receipt covers this one deal. It is not a record of everything the agent did."

// dealDidLine says what kind of evidence the "did" part is. What was asked,
// proposed and approved happened in the conversation on the agent's device,
// so a seal there is the right record of it; what the agent did happened
// elsewhere, so the seal is the agent's own report unless an independent
// source (such as the merchant's own email) is attached. sources names the
// independent sources a deal recorded for what the agent did.
func dealDidLine(sources []string) string {
	const conversation = "What you asked, what was proposed and what you approved are sealed on the agent's device, where they happened. "
	if len(sources) == 0 {
		return conversation + "What the agent did is the agent's own report: no independent source, such as the merchant's own email, is attached."
	}
	return conversation + "What the agent did is the agent's own report, with an independent source attached: " + strings.Join(sources, "; ") + "."
}

// dealDidSources lists the independent sources a deal recorded for what the
// agent did: the merchant's own emails that are merchant-confirmed.
func dealDidSources(events []sealedEvent) []string { return merchantDidSources(events) }

// dealDidLineOf reads the did line the report bundle carries, so the page and
// the email say the same thing.
func dealDidLineOf(b map[string]interface{}) string {
	ext, _ := b["extensions"].(map[string]interface{})
	deal, _ := ext["x-deal-v0"].(map[string]interface{})
	if line, ok := deal["did_line"].(string); ok && line != "" {
		return line
	}
	return dealDidLine(nil)
}

// dealAssurance names the rung the report stands on, from the bundle itself,
// and says the witness state truthfully. "witnessed" needs a receipt in the
// bundle: on the deal's own checkpoint, or (the default) on the cadence
// checkpoint the deal's checkpoint is anchored in. Otherwise the record is
// sealed by the agent's own device, and the text says whether it is still
// scheduled for the next tick or waiting for the witness.
func dealAssurance(b map[string]interface{}) map[string]any {
	out := dealAssuranceRung(b)
	if line := dealProducedByLine(b); line != "" {
		out["text"] = out["text"].(string) + " " + line
		out["produced_by"] = line
	}
	ext, _ := b["extensions"].(map[string]interface{})
	deal, _ := ext["x-deal-v0"].(map[string]interface{})
	if line, _ := deal["instructions"].(string); line != "" {
		out["text"] = out["text"].(string) + " " + line
		out["instructions"] = line
	}
	return out
}

// dealProducedByLine is "Produced by capsulectl <version> (<commit>)." from
// the bundle's x-deal-v0 extension, or "" when it names none.
func dealProducedByLine(b map[string]interface{}) string {
	ext, _ := b["extensions"].(map[string]interface{})
	deal, _ := ext["x-deal-v0"].(map[string]interface{})
	list, _ := deal["produced_by"].([]interface{})
	var names []string
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "Produced by " + strings.Join(names, ", then ") + "."
}

func dealAssuranceRung(b map[string]interface{}) map[string]any {
	ext, _ := b["extensions"].(map[string]interface{})
	cadence, _ := ext[dealCadenceExtension].(map[string]interface{})
	state, _ := cadence["state"].(string)
	cp, _ := b["checkpoint"].(map[string]interface{})
	witnesses, _ := cp["witnesses"].([]interface{})
	if state == "witnessed" {
		inner, _ := cadence["cadence"].(map[string]interface{})
		witnesses, _ = inner["witnesses"].([]interface{})
	}
	sealed := "Sealed by my agent: tamper-evident against ourselves and the agent, not non-repudiation. The key that sealed it is on the agent's own device; it does not cover the agent host's own records. "
	if len(witnesses) == 0 {
		out := map[string]any{"rung": "sealed", "witness_state": "not_configured"}
		switch state {
		case "scheduled":
			out["witness_state"] = "scheduled"
			sealed += "Witness pending. A deal is not witnessed at the moment it happens: its checkpoint goes to the witness at the next tick of this profile's cadence" + cadencePhrase(cadence) + ", and only then can it be witnessed. Until then it is sealed on this device only. "
		case "pending":
			out["witness_state"] = "pending"
			sealed += "Witness pending. This checkpoint was sent at a cadence tick, but no receipt has come back yet; it is retried at every tick. "
			if text, ok := cadence["text"].(string); ok && cadence["reason"] == "network_consent_needed" {
				out["witness_reason"] = "network_consent_needed"
				sealed += "Witness " + text + " "
			}
		}
		out["text"] = sealed + dealDidLineOf(b)
		return out
	}
	entry, _ := witnesses[0].(map[string]interface{})
	host := fmt.Sprint(entry["ts_url"])
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	// When the checkpoint was cut: the tick's time, coarsened to the minute.
	// A receipt on the deal's own checkpoint (no tick) has no such time.
	when := "a checkpoint covering these steps, sent at a cadence tick after them"
	out := map[string]any{"rung": "witnessed", "witness_state": "witnessed", "witness": host}
	if at, _ := cadence["checkpoint_at"].(string); at != "" {
		when = "this deal's checkpoint, cut at " + at + " at a cadence tick after the deal's steps"
		out["checkpoint_at"] = at
	}
	out["text"] = fmt.Sprintf(
		"Witnessed: %s, an independent log, signed a receipt for %s: the record existed, unchanged, by then. It does not confirm what the agent did. The receipt is in the attached bundle; check it with capsulectl verify --bundle bundle.json --witness-directory DIRECTORY.json, using a witness directory you trust. %s", host, when, dealDidLineOf(b))
	return out
}

type dealEmailView struct {
	Demo          bool
	Scope         string
	Asked         string
	Outcome       string
	Assurance     string
	Countersign   string
	Did           []dealReportItem
	Anomalies     []dealReportItem
	Merchant      []dealMerchantRow
	Deadlines     []dealDeadline
	Cancellations []dealCancellation
	Lifecycle     dealLifecycle
	EmailScope    string
	Steps         int
	VerifyLine    string
	NotClaimed    []string
	CheckedNote   string
	// MerchantNote says what stands behind the merchant section, once.
	MerchantNote string
	DeadlineNote string
}

const dealEmailMerchantNote = "Two different things stand behind each email. The merchant's signature, checked against the merchant's key saved when the email was sealed, shows what the merchant sent, independent of the agent. Our seal shows this device kept these exact bytes from that time on; it is our own record. Amounts, dates and order numbers are read from the email by capsulectl and may be misread."

var dealEmailNotClaimed = []string{
	"Tamper-evident against ourselves and the agent, not non-repudiation: it shows the record was not changed after it was made, not who made it.",
	"It covers this skill's own records only, never the agent host's own store: a change there is not detected.",
	"It records what the agent reported doing; it does not prove what the merchant charged or shipped.",
	"Calling the deal check is advisory: steps the agent never sealed are not in it.",
}

const dealEmailVerify = "capsulectl verify --bundle bundle.json"

var dealEmailHTML = template.Must(template.New("email").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Deal receipt</title></head>
<body style="margin:0;padding:16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#1a1a1a;background:#ffffff;">
<div style="max-width:640px;margin:0 auto;">
<h1 style="font-size:20px;margin:0 0 8px;">Deal receipt{{if .Demo}} <span style="font-size:13px;border:1px solid #b45309;color:#b45309;padding:0 6px;border-radius:4px;">DEMO</span>{{end}}</h1>
<p style="margin:0 0 8px;font-weight:600;">{{.Scope}}</p>
<p style="margin:0 0 8px;color:#444;">{{.Assurance}}</p>
<p style="margin:0 0 16px;color:#444;">{{.Countersign}}</p>
<h2 style="font-size:16px;margin:16px 0 4px;">What you asked</h2>
<p style="margin:0;">&ldquo;{{.Asked}}&rdquo;</p>
<h2 style="font-size:16px;margin:16px 0 4px;">What the agent did</h2>
<ul style="margin:0;padding-left:20px;">{{range .Did}}<li>{{.Text}}</li>{{else}}<li>Nothing yet.</li>{{end}}</ul>
<p style="margin:4px 0 0;color:#444;">Outcome: {{.Outcome}} &middot; {{.Steps}} sealed steps</p>
<h2 style="font-size:16px;margin:16px 0 4px;">Anomalies</h2>
<ul style="margin:0;padding-left:20px;">{{range .Anomalies}}<li>{{if .Side}}{{.Side}} side: {{end}}{{.Text}}</li>{{else}}<li>None found.</li>{{end}}</ul>
<h2 style="font-size:16px;margin:16px 0 4px;">Where this deal stands</h2>
<p style="margin:0 0 4px;">{{.Lifecycle.Text}}</p>
{{if .Lifecycle.Later}}<ul style="margin:0;padding-left:20px;">{{range .Lifecycle.Later}}<li>{{.At}} &middot; confirms the close: {{.Text}}</li>{{end}}</ul>{{end}}
<p style="margin:4px 0 0;color:#444;">{{.Lifecycle.MayChange}} (as of {{.Lifecycle.AsOf}})</p>
{{if .Deadlines}}<h2 style="font-size:16px;margin:16px 0 4px;">Cancel-by dates</h2>
<ul style="margin:0;padding-left:20px;">{{range .Deadlines}}<li>{{.CancelBy}} &middot; {{.Marking}}: {{.Text}}. {{.Holds}}</li>{{end}}</ul>
<p style="margin:4px 0 0;color:#444;">{{.DeadlineNote}}</p>{{end}}
{{range .Cancellations}}<h2 style="font-size:16px;margin:16px 0 4px;">Your cancellation</h2>
<p style="margin:0 0 4px;font-weight:600;">What this shows</p>
<ul style="margin:0;padding-left:20px;">{{range .Proven}}<li>{{.}}</li>{{end}}</ul>
<p style="margin:8px 0 4px;font-weight:600;">What this does not show</p>
<ul style="margin:0;padding-left:20px;color:#444;">{{range .NotProven}}<li>{{.}}</li>{{end}}</ul>{{end}}
{{if .Merchant}}<h2 style="font-size:16px;margin:16px 0 4px;">The merchant's own email</h2>
<p style="margin:0 0 4px;font-weight:600;">{{.EmailScope}}</p>
<p style="margin:0 0 8px;color:#444;">{{.MerchantNote}}</p>
{{range .Merchant}}<div style="border:1px solid #d9d9e0;border-radius:6px;padding:8px 12px;margin:8px 0;">
<p style="margin:0 0 4px;font-weight:600;">{{if .OrderID}}Order {{.OrderID}}{{else}}Merchant email{{end}}</p>
<p style="margin:0 0 4px;{{if .Verified}}color:#1d6b35;{{else}}color:#9a3b00;{{end}}">Merchant's signature: {{.MerchantSays}}</p>
<p style="margin:0 0 4px;color:#444;">Our seal: {{.WeSay}}</p>
{{if eq .KeySource "supplied"}}<p style="margin:0 0 4px;color:#9a3b00;">The merchant's key was supplied by hand, not read from the merchant's DNS.</p>{{end}}
<table style="border-collapse:collapse;width:100%;font-size:14px;">
{{if .Approved}}<tr><th style="text-align:left;padding:2px 8px 2px 0;color:#444;font-weight:600;">You approved{{if .ApprovedBasis}} ({{.ApprovedBasis}}){{end}}</th><td>{{.Approved}}</td></tr>{{end}}
{{if .AgentReported}}<tr><th style="text-align:left;padding:2px 8px 2px 0;color:#444;font-weight:600;">The agent reported paying</th><td>{{.AgentReported}}</td></tr>{{end}}
{{if .Charged}}<tr><th style="text-align:left;padding:2px 8px 2px 0;color:#444;font-weight:600;">The merchant's email says (read from the email)</th><td>{{.Charged}}</td></tr>{{end}}
{{if .ChargedOn}}<tr><th style="text-align:left;padding:2px 8px 2px 0;color:#444;font-weight:600;">Charged on (the email's date)</th><td>{{.ChargedOn}}</td></tr>{{end}}
{{if .CancelBy}}<tr><th style="text-align:left;padding:2px 8px 2px 0;color:#444;font-weight:600;">Cancel by (read from the email)</th><td>{{.CancelBy}}</td></tr>{{end}}
{{if .Domains}}<tr><th style="text-align:left;padding:2px 8px 2px 0;color:#444;font-weight:600;">Signing domain</th><td>{{.Domains}}</td></tr>{{end}}
{{if .KeySize}}<tr><th style="text-align:left;padding:2px 8px 2px 0;color:#444;font-weight:600;">Merchant's key</th><td>{{.KeySize}}, sealed when the email was sealed</td></tr>{{end}}
</table></div>{{end}}{{end}}
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
	view.Scope = dealScopeLine
	view.VerifyLine = dealEmailVerify
	view.NotClaimed = dealEmailNotClaimed
	view.EmailScope = emailScopeLine
	view.MerchantNote = dealEmailMerchantNote
	view.DeadlineNote = deadlineNotEnforced
	view.CheckedNote = "This copy cannot check itself: mail apps do not run scripts. Open the attached receipt.html in a browser, where it checks every sealed step offline, or save bundle.json and run:"
	asked := view.Asked
	if r := []rune(asked); len(r) > 60 {
		asked = string(r[:59]) + "…"
	}
	subject = "Deal receipt: " + asked
	if view.Demo {
		subject = "DEMO · " + subject
	}
	if view.Countersign == "" {
		view.Countersign = dealNotCountersignedText
	}
	var tb strings.Builder
	if view.Demo {
		tb.WriteString("DEMO\n\n")
	}
	fmt.Fprintf(&tb, "%s\n\nWhat you asked: \"%s\"\n\n%s\n\n%s\n\nWhat the agent did:\n", view.Scope, view.Asked, view.Assurance, view.Countersign)
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
	fmt.Fprintf(&tb, "\nWhere this deal stands:\n%s\n", view.Lifecycle.Text)
	for _, l := range view.Lifecycle.Later {
		fmt.Fprintf(&tb, "- %s · confirms the close: %s\n", l.At, l.Text)
	}
	fmt.Fprintf(&tb, "%s (as of %s)\n", view.Lifecycle.MayChange, view.Lifecycle.AsOf)
	if len(view.Deadlines) > 0 {
		tb.WriteString("\nCancel-by dates:\n")
		for _, d := range view.Deadlines {
			fmt.Fprintf(&tb, "- %s · %s: %s. %s\n", d.CancelBy, d.Marking, d.Text, d.Holds)
		}
		fmt.Fprintf(&tb, "%s\n", view.DeadlineNote)
	}
	for _, c := range view.Cancellations {
		tb.WriteString("\nYour cancellation\nWhat this shows:\n")
		for _, p := range c.Proven {
			fmt.Fprintf(&tb, "- %s\n", p)
		}
		tb.WriteString("What this does not show:\n")
		for _, p := range c.NotProven {
			fmt.Fprintf(&tb, "- %s\n", p)
		}
	}
	if len(view.Merchant) > 0 {
		fmt.Fprintf(&tb, "\nThe merchant's own email:\n%s\n%s\n", view.EmailScope, view.MerchantNote)
		for _, m := range view.Merchant {
			title := "Merchant email"
			if m.OrderID != "" {
				title = "Order " + m.OrderID
			}
			fmt.Fprintf(&tb, "\n%s\n- Merchant's signature: %s\n- Our seal: %s\n", title, m.MerchantSays, m.WeSay)
			if m.KeySource == "supplied" {
				tb.WriteString("- The merchant's key was supplied by hand, not read from the merchant's DNS.\n")
			}
			approved, key := "You approved", ""
			if m.ApprovedBasis != "" {
				approved += " (" + m.ApprovedBasis + ")"
			}
			if m.KeySize != "" {
				key = m.KeySize + ", sealed when the email was sealed"
			}
			for _, f := range []struct{ label, value string }{
				{approved, m.Approved},
				{"The agent reported paying", m.AgentReported},
				{"The merchant's email says (read from the email)", m.Charged},
				{"Charged on (the email's date)", m.ChargedOn},
				{"Cancel by (read from the email)", m.CancelBy},
				{"Signing domain", m.Domains},
				{"Merchant's key", key},
			} {
				if f.value != "" {
					fmt.Fprintf(&tb, "- %s: %s\n", f.label, f.value)
				}
			}
		}
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

// cadencePhrase is " (every 5m, give or take 2m)" from the witness state the
// report carries, or "" when it names none.
func cadencePhrase(cadence map[string]interface{}) string {
	if words, _ := cadence["cadence"].(string); words != "" {
		return " (" + words + ")"
	}
	return ""
}
