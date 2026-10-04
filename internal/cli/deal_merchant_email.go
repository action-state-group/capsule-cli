package cli

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/spf13/cobra"
	"golang.org/x/net/publicsuffix"
)

// A merchant's own confirmation email is the one piece of deal evidence that
// does not reduce to "the agent says so": the merchant's mail server signs it
// with a DKIM key published in the merchant's DNS, and that signature survives
// being relayed by an agent nobody trusts. Two attestations, never one:
//
//   - our seal says what OUR side saw: these exact bytes, at this time;
//   - the DKIM signature says what the MERCHANT sent. Only this one is
//     independent of the agent and of this device.
//
// DKIM keys rotate and are revoked, so a signature that verifies today may be
// uncheckable next month. The key record is therefore captured from DNS when
// the email is sealed and sealed with it, and the step is checkpointed at once;
// the checkpoint reaches the witness, when one is configured, at the next
// cadence tick (`deal tick`). Verification afterwards
// runs offline against the sealed key record, never against live DNS.
//
// DKIM verification is github.com/emersion/go-msgauth/dkim (MIT). Nothing here
// implements cryptography.

// maxEmail bounds a sealed message. Confirmation emails are well under this.
const maxEmail = 10 << 20

// merchantEmail is a sealed merchant email as the local store keeps it. Raw is the
// RFC 822 message exactly as received, headers intact; it never leaves the
// device and is never in a record or a report.
type merchantEmail struct {
	Raw  []byte          `json:"raw"`
	Keys []dkimKeyRecord `json:"keys"`
	// KeySource is "dns" when the key records were fetched from DNS at seal
	// time, or "supplied" when the caller handed them over (a test, or a
	// machine with no resolver); a supplied key is not evidence of what the
	// merchant published.
	KeySource string `json:"key_source"`
	// DMARC holds the sender domain's DMARC records as they stood at seal
	// time (_dmarc.<From domain>, then its organizational domain), with the
	// same source rule as the keys. Nil when they were not captured.
	DMARC  *dmarcCapture        `json:"dmarc,omitempty"`
	DKIM   merchantEmailVerdict `json:"dkim"`
	Parsed merchantEmailParsed  `json:"parsed"`
}

type dmarcCapture struct {
	Source  string          `json:"source"`
	Records []dkimKeyRecord `json:"records"`
}

// dkimKeyRecord is one DNS TXT key record (selector._domainkey.domain) as it
// stood when the email was sealed. A name that had no record is kept with no
// TXT, so a later check fails the same way.
type dkimKeyRecord struct {
	Name string   `json:"name"`
	TXT  []string `json:"txt"`
}

// merchantEmailVerdict is the DKIM result in plain terms. Result is "pass" when
// at least one signature verified, "fail" when signatures were present and
// none verified, "none" when the message carried no signature. Merchant is
// true only when a passing signature is DMARC-aligned with the From domain
// (the same organizational domain, or the exact domain under adkim=s): a
// passing signature from a mailing service alone shows the service relayed
// it, not that the merchant sent it. Policy is the sender domain's published
// DMARC policy (reject, quarantine, none), "absent" when it publishes none,
// or "not_captured". It is shown with every result that is not confirmed and
// never changes what is sealed: a message that fails is still evidence the
// user received it.
type merchantEmailVerdict struct {
	Result     string          `json:"result"`
	Merchant   bool            `json:"merchant"`
	FromDomain string          `json:"from_domain,omitempty"`
	Policy     string          `json:"dmarc_policy"`
	Signatures []dealSignature `json:"signatures"`
}

type dealSignature struct {
	Domain string   `json:"domain"`
	Result string   `json:"result"`
	Reason string   `json:"reason,omitempty"`
	Signed []string `json:"signed_headers,omitempty"`
}

// merchantEmailParsed is read from the email by merchant-agnostic heuristics. It
// is a best effort, always labelled "parsed", and never the attestation:
// the DKIM signature covers the bytes, not this reading of them.
type merchantEmailParsed struct {
	OrderID string `json:"order_id,omitempty"`
	// OrderIDLabel is the word the id was found under: "order", or one of
	// booking, confirmation, reservation, receipt, pnr, record locator.
	// Only an "order" id can ever be shared (see shareableOrderID).
	OrderIDLabel string `json:"order_id_label,omitempty"`
	TotalMinor   *int64 `json:"total_minor,omitempty"`
	Currency     string `json:"currency,omitempty"`
	CancelBy     string `json:"cancel_by,omitempty"` // YYYY-MM-DD
	// ChargeAfterCancelBy is set when the email says no charge comes before
	// the cancel-by date (a trial, or "cancel by ... to avoid being charged").
	// Only then is a charge dated before it a mismatch.
	ChargeAfterCancelBy bool                `json:"charge_after_cancel_by,omitempty"`
	SentAt              string              `json:"sent_at,omitempty"` // the Date header, UTC
	Subject             string              `json:"subject,omitempty"`
	Items               []merchantEmailItem `json:"items,omitempty"`
}

type merchantEmailItem struct {
	Text     string `json:"text"`
	Quantity int64  `json:"quantity,omitempty"`
}

func emailDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keyRecordsDigest is the SHA-256 of the JCS form of the sealed key records:
// what a reader compares against to know which keys the verdict used.
func keyRecordsDigest(keys []dkimKeyRecord) (string, error) {
	list := make([]interface{}, len(keys))
	for i, k := range keys {
		txt := make([]interface{}, len(k.TXT))
		for j, t := range k.TXT {
			txt[j] = t
		}
		list[i] = map[string]interface{}{"name": k.Name, "txt": txt}
	}
	return canonical.JSONDigest(list)
}

// dkimLookup resolves key records at seal time, with a deadline.
var dkimLookup = func(name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// captureEmail verifies raw against DNS as it stands now (or against supplied
// key records) and records every key record it read, so the same verdict can
// be reached later with no network.
func captureEmail(raw []byte, supplied []dkimKeyRecord) (*merchantEmail, error) {
	if len(raw) == 0 || len(raw) > maxEmail {
		return nil, inputError("the email must be a raw RFC 822 message (.eml) of at most 10 MB")
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
		// The parser's own message can quote a header line (an address):
		// it stays in the chain, never in the shown reason.
		return nil, errors.Join(inputError("the email is not a raw RFC 822 message with its headers intact (a header line is malformed, or the header block does not end with a blank line)"), err)
	}
	e := &merchantEmail{Raw: raw, KeySource: "dns"}
	var fetchErr error
	fetch := func(name, what string) ([]string, error) {
		txt, err := dkimLookup(name)
		var dnsErr *net.DNSError
		if err != nil && !(errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
			// Not sealed: a record we could not read is not a record that is absent.
			fetchErr = errors.Join(fetchErr, inputError("could not fetch the "+what+" "+name+" ("+err.Error()+"); retry, or pass --key-record"))
			return nil, err
		}
		return txt, nil
	}
	lookup := func(name string) ([]string, error) {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		for _, k := range e.Keys {
			if k.Name == name {
				return k.TXT, nil
			}
		}
		txt, err := fetch(name, "DKIM key record")
		if err != nil {
			return nil, err
		}
		e.Keys = append(e.Keys, dkimKeyRecord{Name: name, TXT: txt})
		return txt, nil
	}
	if supplied != nil {
		e.KeySource = "supplied"
		e.Keys = supplied
		lookup = sealedLookup(supplied)
		e.DMARC = dmarcSupplied
	} else if from := fromDomain(raw); from != "" {
		e.DMARC = &dmarcCapture{Source: "dns", Records: []dkimKeyRecord{}}
		for _, name := range dmarcNames(from) {
			txt, err := fetch(name, "DMARC record")
			if err != nil {
				break
			}
			e.DMARC.Records = append(e.DMARC.Records, dkimKeyRecord{Name: name, TXT: txt})
		}
	}
	verdict, err := verifyDKIM(raw, lookup, e.DMARC)
	if err = errors.Join(fetchErr, err); err != nil {
		return nil, err
	}
	e.DKIM = verdict
	e.Parsed = parseMerchantEmail(raw)
	return e, nil
}

// sealedLookup answers key queries from sealed records only. A name that was
// not sealed has no key: verification never falls back to the network.
func sealedLookup(keys []dkimKeyRecord) func(string) ([]string, error) {
	return func(name string) ([]string, error) {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		for _, k := range keys {
			if k.Name == name {
				return k.TXT, nil
			}
		}
		return nil, nil
	}
}

// verifyDKIM runs a standard DKIM verification with the key lookup injected.
func verifyDKIM(raw []byte, lookup func(string) ([]string, error), dmarc *dmarcCapture) (merchantEmailVerdict, error) {
	v := merchantEmailVerdict{Result: "none", Signatures: []dealSignature{}, FromDomain: fromDomain(raw)}
	pol := dmarcPolicyOf(v.FromDomain, dmarc)
	v.Policy = pol.policy
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: lookup, MaxVerifications: 8})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		return v, errors.Join(inputError("could not read the email's DKIM-Signature headers: one is malformed"), err)
	}
	for _, x := range verifs {
		s := dealSignature{Domain: strings.ToLower(x.Domain), Result: "pass", Signed: x.HeaderKeys}
		if x.Err != nil {
			s.Result, s.Reason = "fail", strings.TrimPrefix(x.Err.Error(), "dkim: ")
		}
		v.Signatures = append(v.Signatures, s)
	}
	if len(v.Signatures) > 0 {
		v.Result = "fail"
	}
	for _, s := range v.Signatures {
		if s.Result != "pass" {
			continue
		}
		v.Result = "pass"
		if dmarcAligned(v.FromDomain, s.Domain, pol.strict) {
			v.Merchant = true
		}
	}
	return v, nil
}

// fromDomain is the domain of the message's From address, lowercased.
func fromDomain(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		return ""
	}
	at := strings.LastIndex(from.Address, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(from.Address[at+1:], "."))
}

// orgDomain is a domain's organizational domain (RFC 7489 section 3.2): its
// registrable part under the public suffix list.
func orgDomain(d string) string {
	if org, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return org
	}
	return d
}

// dmarcNames are the names DMARC policy discovery reads: the From domain's
// own record, then its organizational domain's.
func dmarcNames(from string) []string {
	names := []string{"_dmarc." + from}
	if org := orgDomain(from); org != from {
		names = append(names, "_dmarc."+org)
	}
	return names
}

// dmarcAligned is DKIM identifier alignment: the same organizational domain
// (relaxed, the default) or the exact domain (adkim=s).
func dmarcAligned(from, signer string, strict bool) bool {
	if from == "" || signer == "" {
		return false
	}
	if strict {
		return from == signer
	}
	return orgDomain(from) == orgDomain(signer)
}

type dmarcPolicy struct {
	policy string
	strict bool
}

// dmarcSupplied marks DMARC as not captured: a hand-supplied key comes with
// no DMARC record from the merchant's DNS.
var dmarcSupplied = &dmarcCapture{Source: "not_captured", Records: []dkimKeyRecord{}}

// dmarcPolicyOf reads the published policy from the sealed DMARC records:
// the From domain's own record, else the organizational domain's (whose sp=
// applies to a subdomain).
func dmarcPolicyOf(from string, c *dmarcCapture) dmarcPolicy {
	if c == nil || c.Source == "not_captured" || from == "" {
		return dmarcPolicy{policy: "not_captured"}
	}
	for i, name := range dmarcNames(from) {
		for _, r := range c.Records {
			if r.Name != name {
				continue
			}
			for _, txt := range r.TXT {
				tags := dmarcTags(txt)
				if tags["v"] != "DMARC1" {
					continue
				}
				p := strings.ToLower(tags["p"])
				if sp := strings.ToLower(tags["sp"]); i > 0 && sp != "" {
					p = sp
				}
				if p != "reject" && p != "quarantine" && p != "none" {
					p = "none"
				}
				return dmarcPolicy{policy: p, strict: strings.EqualFold(tags["adkim"], "s")}
			}
		}
	}
	return dmarcPolicy{policy: "absent"}
}

func dmarcTags(txt string) map[string]string {
	tags := map[string]string{}
	for _, part := range strings.Split(txt, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			tags[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return tags
}

// dmarcDigest is the SHA-256 of the JCS array of the sealed DMARC records.
func (c *dmarcCapture) digest() (string, error) {
	if c == nil || c.Source == "not_captured" {
		return "", nil
	}
	return keyRecordsDigest(c.Records)
}

// recheck re-verifies a sealed email, or another copy of it, offline against
// the sealed key records.
func (e *merchantEmail) recheck(raw []byte) (merchantEmailVerdict, error) {
	return verifyDKIM(raw, sealedLookup(e.Keys), e.DMARC)
}

// parseKeyRecord reads a key record file: the TXT value, or a zone-file line
// `selector._domainkey.example.com. IN TXT "v=DKIM1; ..."`. A bare value takes
// its name from the email's first signature.
func parseKeyRecord(b []byte, raw []byte) ([]dkimKeyRecord, error) {
	name, text := zoneTXT(b)
	if !strings.Contains(text, "p=") {
		return nil, inputError("--key-record must hold a DKIM key record (v=DKIM1; k=...; p=...)")
	}
	if name == "" {
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			return nil, inputError("the email is not a raw RFC 822 message")
		}
		sig := msg.Header.Get("DKIM-Signature")
		d, s := dkimTag(sig, "d"), dkimTag(sig, "s")
		if d == "" || s == "" {
			return nil, inputError("the email has no DKIM-Signature to name the key record by; give the record's name in zone-file form")
		}
		name = s + "._domainkey." + d
	}
	return []dkimKeyRecord{{Name: strings.ToLower(strings.TrimSuffix(name, ".")), TXT: []string{text}}}, nil
}

// parseDMARCRecord reads a DMARC record file: the TXT value, or a zone-file
// line `_dmarc.example.com. IN TXT "v=DMARC1; p=reject"`. A bare value is
// named for the From domain.
func parseDMARCRecord(b []byte, from string) ([]dkimKeyRecord, error) {
	name, text := zoneTXT(b)
	if dmarcTags(text)["v"] != "DMARC1" {
		return nil, inputError("--dmarc-record must hold a DMARC record (v=DMARC1; p=...)")
	}
	if name == "" {
		if from == "" {
			return nil, inputError("the email has no From domain to name the DMARC record by")
		}
		name = "_dmarc." + from
	}
	return []dkimKeyRecord{{Name: name, TXT: []string{text}}}, nil
}

// zoneTXT splits a TXT value, or a zone-file TXT line, into its name (if
// given) and its joined value.
func zoneTXT(b []byte) (name, text string) {
	text = strings.TrimSpace(string(b))
	if i := strings.Index(strings.ToUpper(text), " TXT "); i > 0 && !strings.HasPrefix(text, "v=") {
		name = strings.ToLower(strings.TrimSuffix(strings.Fields(text[:i])[0], "."))
		text = strings.TrimSpace(text[i+5:])
	}
	if strings.HasPrefix(text, `"`) {
		var parts []string
		for _, m := range quotedPart.FindAllStringSubmatch(text, -1) {
			parts = append(parts, m[1])
		}
		text = strings.Join(parts, "")
	}
	return name, text
}

var quotedPart = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

func dkimTag(sig, tag string) string {
	for _, p := range strings.Split(sig, ";") {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.TrimSpace(k) == tag {
			return strings.Join(strings.Fields(v), "")
		}
	}
	return ""
}

// --- parsing: merchant-agnostic heuristics, labelled "parsed" everywhere ---

// emailText returns the readable text of a message: its text/plain parts, or
// its text/html parts with the markup stripped when there is no plain part.
func emailText(raw []byte) (*mail.Message, string) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, ""
	}
	var plain, htmlText []string
	var walk func(header map[string][]string, body io.Reader, depth int)
	walk = func(header map[string][]string, body io.Reader, depth int) {
		get := func(k string) string {
			if v := header[k]; len(v) > 0 {
				return v[0]
			}
			return ""
		}
		ctype, params, err := mime.ParseMediaType(get("Content-Type"))
		if err != nil {
			ctype = "text/plain"
		}
		if strings.HasPrefix(ctype, "multipart/") && depth < 8 {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				part, err := mr.NextRawPart()
				if err != nil {
					return
				}
				walk(part.Header, part, depth+1)
			}
		}
		if !strings.HasPrefix(ctype, "text/") {
			return
		}
		var r io.Reader = io.LimitReader(body, maxEmail)
		switch strings.ToLower(strings.TrimSpace(get("Content-Transfer-Encoding"))) {
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, newlineStripper{r})
		}
		b, _ := io.ReadAll(r)
		switch ctype {
		case "text/plain":
			plain = append(plain, string(b))
		case "text/html":
			htmlText = append(htmlText, stripHTML(string(b)))
		}
	}
	walk(msg.Header, msg.Body, 0)
	if len(plain) > 0 {
		return msg, strings.Join(plain, "\n")
	}
	return msg, strings.Join(htmlText, "\n")
}

type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	k, err := n.r.Read(p)
	out := p[:0]
	for _, c := range p[:k] {
		if c != '\r' && c != '\n' {
			out = append(out, c)
		}
	}
	return len(out), err
}

var (
	htmlDrop  = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)>`)
	htmlBreak = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/li|/h[1-6]|/table)\b[^>]*>`)
	htmlCell  = regexp.MustCompile(`(?i)</t[dh]>`)
	htmlTag   = regexp.MustCompile(`<[^>]*>`)
	hspace    = regexp.MustCompile(`[ \t\x{a0}]+`)
)

func stripHTML(s string) string {
	s = htmlDrop.ReplaceAllString(s, "")
	s = htmlBreak.ReplaceAllString(s, "\n")
	s = htmlCell.ReplaceAllString(s, " ")
	s = html.UnescapeString(htmlTag.ReplaceAllString(s, ""))
	return hspace.ReplaceAllString(s, " ")
}

var (
	orderIDLine = regexp.MustCompile(`(?i)\b(order|confirmation|booking|reservation|receipt|pnr|record locator)\s*(?:number|no\.?|id|#|reference|ref\.?)?\s*[:#]?\s*#?\s*([A-Z0-9][A-Z0-9-]{3,39})\b`)
	// A booking, confirmation, reservation or PNR code works like a password
	// with a surname: it is kept, never shared. Only an order number may be.
	shareableIDLabel = map[string]bool{"order": true}
	totalLine        = regexp.MustCompile(`(?i)\b(grand total|order total|total charged|amount charged|total paid|amount paid|you paid|total)\b[^0-9\n]{0,24}?(?:([A-Z]{3})\s*)?([$€£¥])?\s*([0-9]{1,3}(?:,[0-9]{3})*|[0-9]+)(?:\.([0-9]{2}))?(?:\s*([A-Z]{3}))?`)
	subtotal         = regexp.MustCompile(`(?i)sub\s*-?\s*total|total\s+(?:items?|savings|discount|tax|before)`)
	cancelLine       = regexp.MustCompile(`(?i)\bcancel(?:l?ation)?\b[^\n.]{0,40}?\b(?:by|before|until|no later than|deadline:?)\s+([^\n]{6,40})`)
	chargeLater      = regexp.MustCompile(`(?i)avoid (being )?charged|before (you are|you're|you get|being) (charged|billed)|won'?t be (charged|billed)|will not be (charged|billed)|free trial|trial (ends|period)|first (charge|payment) (is|will be) on`)
	itemQty          = regexp.MustCompile(`(?im)^\s*(.{2,80}?)\s+(?:qty|quantity)\s*[:x]?\s*(\d{1,4})\b`)
	itemTimes        = regexp.MustCompile(`(?im)^\s*(\d{1,4})\s*[x×]\s+(.{2,80}?)\s*$`)
	orderIDBad       = map[string]bool{"CONFIRMATION": true, "NUMBER": true, "DETAILS": true, "SUMMARY": true, "TOTAL": true, "STATUS": true}
)

var symbolCurrency = map[string]string{"$": "USD", "€": "EUR", "£": "GBP", "¥": "JPY"}

// parseMerchantEmail reads the order id, total, cancel-by date, items and
// sent time. It guesses; everything it returns is shown as "parsed".
func parseMerchantEmail(raw []byte) merchantEmailParsed {
	var p merchantEmailParsed
	msg, text := emailText(raw)
	if msg == nil {
		return p
	}
	if t, err := msg.Header.Date(); err == nil {
		p.SentAt = t.UTC().Format("2006-01-02T15:04:05Z")
	}
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		subject = msg.Header.Get("Subject")
	}
	p.Subject = strings.TrimSpace(subject)
	for _, src := range []string{p.Subject, text} {
		for _, m := range orderIDLine.FindAllStringSubmatch(src, -1) {
			id := strings.ToUpper(m[2])
			if !orderIDBad[id] && strings.ContainsAny(id, "0123456789") {
				p.OrderID, p.OrderIDLabel = m[2], strings.ToLower(m[1])
				break
			}
		}
		if p.OrderID != "" {
			break
		}
	}
	// The strongest total label wins; among equals, the last one (the final
	// line of a receipt). "Subtotal" and "total tax" are never the total.
	rank := map[string]int{"grand total": 3, "order total": 3, "total charged": 3, "amount charged": 3, "total paid": 3, "amount paid": 3, "you paid": 3, "total": 1}
	best := 0
	for _, line := range strings.Split(text, "\n") {
		if subtotal.MatchString(line) {
			continue
		}
		for _, m := range totalLine.FindAllStringSubmatch(line, -1) {
			r := rank[strings.ToLower(m[1])]
			if r < best {
				continue
			}
			whole, err := strconv.ParseInt(strings.ReplaceAll(m[4], ",", ""), 10, 64)
			if err != nil || (m[2] == "" && m[3] == "" && m[6] == "" && m[5] == "") {
				continue
			}
			cents := int64(0)
			if m[5] != "" {
				cents, _ = strconv.ParseInt(m[5], 10, 64)
			}
			best = r
			p.Currency = strings.ToUpper(firstNonEmpty(m[2], m[6], symbolCurrency[m[3]]))
			minor := whole*100 + cents
			if p.Currency == "JPY" {
				minor = whole
			}
			p.TotalMinor = &minor
		}
	}
	if m := cancelLine.FindStringSubmatch(text); m != nil {
		p.CancelBy = parseLooseDate(m[1])
		p.ChargeAfterCancelBy = p.CancelBy != "" && chargeLater.MatchString(text)
	}
	for _, m := range itemQty.FindAllStringSubmatch(text, 20) {
		q, _ := strconv.ParseInt(m[2], 10, 64)
		p.Items = append(p.Items, merchantEmailItem{Text: strings.TrimSpace(m[1]), Quantity: q})
	}
	for _, m := range itemTimes.FindAllStringSubmatch(text, 20) {
		q, _ := strconv.ParseInt(m[1], 10, 64)
		p.Items = append(p.Items, merchantEmailItem{Text: strings.TrimSpace(m[2]), Quantity: q})
	}
	return p
}

var looseDateLayouts = []string{
	"January 2, 2006", "January 2 2006", "Jan 2, 2006", "Jan 2 2006", "2 January 2006", "2 Jan 2006",
	"Monday, January 2, 2006", "Mon, Jan 2, 2006", "2006-01-02", "01/02/2006",
}

var dateish = regexp.MustCompile(`(?i)((?:mon|tue|wed|thu|fri|sat|sun)[a-z]*,?\s+)?([a-z]{3,9}\.?\s+\d{1,2},?\s+\d{4}|\d{1,2}\s+[a-z]{3,9}\.?\s+\d{4}|\d{4}-\d{2}-\d{2}|\d{1,2}/\d{1,2}/\d{4})`)

// parseLooseDate reads the first date in s as YYYY-MM-DD, or "".
func parseLooseDate(s string) string {
	m := dateish.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	d := strings.NewReplacer(".", "", ",", ",").Replace(strings.TrimSpace(m[2]))
	for _, layout := range looseDateLayouts {
		if t, err := time.Parse(layout, d); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// shareableOrderID is the one id a shared copy may carry from a merchant
// email, and only the counterparty's copy: an id found under the word
// "order", in an email whose DKIM signature checks out against the sealed
// key, signed by the From domain, and that signer being the deal's own
// counterparty from first contact. Booking, confirmation, reservation and
// PNR-style codes are never shared, and neither is any id from an email
// that does not check out. It returns "" when the id stays withheld.
func (m *merchantEmail) shareableOrderID(first dealWho) string {
	if m == nil || m.Parsed.OrderID == "" || !shareableIDLabel[m.Parsed.OrderIDLabel] {
		return ""
	}
	if m.DKIM.Result != "pass" || !m.DKIM.Merchant {
		return ""
	}
	if same, known := signerMatchesBaseline(m.DKIM, first); !known || !same {
		return ""
	}
	return m.Parsed.OrderID
}

// emailSigner is the domain whose passing signature the verdict rests on: the
// merchant-aligned one when there is one.
func (v merchantEmailVerdict) signer() string {
	var any string
	for _, s := range v.Signatures {
		if s.Result != "pass" {
			continue
		}
		if dmarcAligned(v.FromDomain, s.Domain, false) {
			return s.Domain
		}
		if any == "" {
			any = s.Domain
		}
	}
	return any
}

// emailVerdictWords says what the DKIM result means, for a reader who has
// never heard of DKIM. It never merges the merchant's attestation with ours.
//
// Only a passing signature aligned with the sender's domain, verified by our
// own offline check, reads as confirmed. Everything else reads "not
// confirmed", says why, and shows the domain's DMARC policy, whatever it is.
func emailVerdictWords(v merchantEmailVerdict) string {
	if v.Result == "pass" && v.Merchant {
		return "merchant-confirmed: the merchant's signature checks out: " + v.signer() + " sent this email, and it has not been changed since"
	}
	var why string
	switch v.Result {
	case "pass":
		why = "signed by " + v.signer() + ", which is not the sender's own domain (" + v.FromDomain + "): this shows who relayed it, not that the merchant sent it"
	case "fail":
		reasons := []string{}
		for _, s := range v.Signatures {
			if s.Reason != "" && !slices.Contains(reasons, s.Reason) {
				reasons = append(reasons, s.Reason)
			}
		}
		why = "the signature does not check out (" + strings.Join(reasons, "; ") + ")"
	default:
		why = "the email carries no signature, so only our own seal stands behind it"
	}
	return "not confirmed: fails DMARC alignment (" + dmarcPolicyWords(v.Policy) + "); " + why
}

func dmarcPolicyWords(policy string) string {
	switch policy {
	case "reject", "quarantine", "none":
		return "domain policy p=" + policy
	case "absent":
		return "the domain publishes no DMARC policy"
	default:
		return "domain policy not captured"
	}
}

// ourSealWords is what our own seal on a merchant email does and does not
// show, said once, next to the merchant's attestation and never merged with it.
const ourSealWords = "our seal shows this device held these exact bytes at this time; it is our own record, not the merchant's"

// An email result states its own scope on its face, as a receipt does
// (dealScopeLine).
const emailScopeLine = "This covers one email from the merchant about this deal. It is not a record of everything the merchant sent or charged."

// merchantDidSources names each sealed merchant email that is
// merchant-confirmed (a passing, aligned signature verified by our own check)
// as an independent source for what the agent did. A sealed email that is not
// confirmed is not a source.
func merchantDidSources(events []sealedEvent) []string {
	var out []string
	for _, se := range events {
		if e := se.Event.Evidence; e != nil && e.Email != nil && e.Verified {
			out = append(out, "the merchant's own email, its signature checked against the merchant's key sealed at "+se.Event.At)
		}
	}
	return out
}

// ErrEmailUnverified is a merchant email whose DKIM signature does not verify
// against the sealed key record, or a copy that is not the sealed one. The
// result was printed.
var ErrEmailUnverified = errors.New("merchant email did not verify against the sealed key record")

// attachEmail reads a raw merchant email and its key records into an evidence
// step. The step counts as verified only when the merchant's own domain
// signed it.
func attachEmail(ev *dealEvidence, emailPath, keyPath, dmarcPath string) error {
	raw, err := readInput(emailPath)
	if err != nil {
		return err
	}
	var supplied []dkimKeyRecord
	if keyPath != "" {
		b, err := readInput(keyPath)
		if err != nil {
			return err
		}
		if supplied, err = parseKeyRecord(b, raw); err != nil {
			return err
		}
	}
	if dmarcPath != "" && keyPath == "" {
		return inputError("--dmarc-record goes with --key-record; without it both are read from DNS")
	}
	m, err := captureEmail(raw, supplied)
	if err != nil {
		return err
	}
	if dmarcPath != "" {
		b, err := readInput(dmarcPath)
		if err != nil {
			return err
		}
		rec, err := parseDMARCRecord(b, m.DKIM.FromDomain)
		if err != nil {
			return err
		}
		m.DMARC = &dmarcCapture{Source: "supplied", Records: rec}
		if m.DKIM, err = m.recheck(raw); err != nil {
			return err
		}
	}
	ev.Email = m
	ev.Verified = m.DKIM.Result == "pass" && m.DKIM.Merchant
	return nil
}

func dealVerifyEmailCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "verify-email", Short: "Re-check a sealed merchant email's DKIM signature offline, against the key record sealed with it", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		copyPath, _ := c.Flags().GetString("email")
		step, _ := c.Flags().GetInt64("step")
		var other []byte
		if copyPath != "" {
			var err error
			if other, err = readInput(copyPath); err != nil {
				return err
			}
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			var results []map[string]any
			ok := true
			for _, se := range events {
				m := se.Event.Evidence
				if se.Event.Kind != "evidence" || m == nil || m.Email == nil || (step != 0 && se.Event.N != step) {
					continue
				}
				e := m.Email
				raw := e.Raw
				r := map[string]any{"step": se.Event.N, "capsule_id": se.CapsuleID, "sealed_at": se.Event.At, "key_source": e.KeySource, "sealed_message_digest": emailDigest(e.Raw), "dkim_at_seal": e.DKIM.Result}
				if other != nil {
					raw = other
					same := emailDigest(other) == emailDigest(e.Raw)
					r["copy_digest"] = emailDigest(other)
					r["copy_is_sealed_message"] = same
					ok = ok && same
				}
				v, err := e.recheck(raw)
				if err != nil {
					return err
				}
				r["dkim"] = v.Result
				r["merchant_signed"] = v.Merchant
				r["signatures"] = v.Signatures
				r["merchant_says"] = emailVerdictWords(v)
				r["we_say"] = ourSealWords
				r["network"] = "not used: checked against the key record sealed at " + se.Event.At
				ok = ok && v.Result == "pass" && v.Merchant
				results = append(results, r)
			}
			if len(results) == 0 {
				return inputError("no sealed merchant email in this deal (or at that step)")
			}
			if err := output(c, map[string]any{"deal_id": dealID, "scope": emailScopeLine, "verified": ok, "emails": results}); err != nil {
				return err
			}
			if !ok {
				return ErrEmailUnverified
			}
			return nil
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().Int64("step", 0, "Only the evidence step with this number")
	cmd.Flags().String("email", "", "Check this copy of the email instead of the sealed bytes (it must match them byte for byte)")
	return cmd
}

// dealMerchantRow is one sealed merchant email beside what the user approved.
// Approved and AgentReported come from our own sealed steps; Charged and the
// rest are parsed from the merchant's email, and the row says whether the
// merchant's signature on that email checked out.
type dealMerchantRow struct {
	Steps         []string `json:"steps"`
	OrderID       string   `json:"order_id,omitempty"`
	Verified      bool     `json:"verified"`
	MerchantSays  string   `json:"merchant_says"`
	WeSay         string   `json:"we_say"`
	Approved      string   `json:"approved,omitempty"`
	ApprovedBasis string   `json:"approved_basis,omitempty"`
	AgentReported string   `json:"agent_reported,omitempty"`
	Charged       string   `json:"charged,omitempty"`
	ChargedOn     string   `json:"charged_on,omitempty"`
	CancelBy      string   `json:"cancel_by,omitempty"`
	Items         []string `json:"items,omitempty"`
	KeySource     string   `json:"key_source"`
	// KeySize is the merchant key the verdict rests on ("RSA 2048-bit",
	// "Ed25519"); SigningDomain is the domain whose signature passed, and
	// MerchantApex the sender's organizational domain. Domains says how the
	// two compare, in words.
	KeySize       string `json:"key_size,omitempty"`
	SigningDomain string `json:"signing_domain,omitempty"`
	MerchantApex  string `json:"merchant_apex,omitempty"`
	Domains       string `json:"domains,omitempty"`
}

// keySize names the sealed key the passing signature from signer used.
func (m *merchantEmail) keySize(signer string) string {
	if signer == "" {
		return ""
	}
	for _, k := range m.Keys {
		if !strings.HasSuffix(k.Name, "._domainkey."+signer) || len(k.TXT) == 0 {
			continue
		}
		tags := dmarcTags(strings.Join(k.TXT, ""))
		der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(tags["p"]), ""))
		if err != nil || len(der) == 0 {
			continue
		}
		if strings.EqualFold(tags["k"], "ed25519") {
			return "Ed25519"
		}
		pub, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			if rk, perr := x509.ParsePKCS1PublicKey(der); perr == nil {
				pub = rk
			}
		}
		if rk, ok := pub.(*rsa.PublicKey); ok {
			size := fmt.Sprintf("RSA %d-bit", rk.N.BitLen())
			if rk.N.BitLen() < 2048 {
				size += " (shorter than the 2048 bits now recommended)"
			}
			return size
		}
	}
	return ""
}

// domainWords compares the signing domain with the merchant's apex domain.
func domainWords(signer, apex string) string {
	switch {
	case signer == "" && apex == "":
		return ""
	case signer == "":
		return "no signature passed; the merchant's domain (apex) is " + apex
	case orgDomain(signer) == apex:
		return "signed by " + signer + ", the merchant's own domain (apex " + apex + ")"
	default:
		return "signed by " + signer + "; the merchant's domain (apex) is " + apex + ": not the same organization"
	}
}

// approvedAmount is what the user approved, from our own sealed steps: the
// amount on the last check that went ahead, else the user's limit, else the
// agreed price. limit is true when only a ceiling is known.
func approvedAmount(events []sealedEvent, state dealState) (amount *int64, currency, basis string, steps []string, limit bool) {
	byID := map[string]sealedEvent{}
	for _, se := range events {
		byID[se.CapsuleID] = se
	}
	currency = state.agreed.Currency
	for i := len(events) - 1; i >= 0; i-- {
		a := events[i].Event.Approval
		if a == nil || !a.Proceed || a.Reason != "" {
			continue
		}
		check, ok := byID[a.Check]
		if !ok || check.Event.Check == nil {
			continue
		}
		snap, ok := byID[check.Event.Check.Snapshot]
		if !ok || snap.Event.Snapshot == nil || snap.Event.Snapshot.AmountMinor == nil {
			continue
		}
		if t := snap.Event.Snapshot.Terms; t != nil && t.Currency != "" {
			currency = t.Currency
		}
		basis = "the amount you approved at the check"
		if a.Approver == "standing_intent" {
			basis = "the amount checked against what you already allowed"
		}
		return snap.Event.Snapshot.AmountMinor, currency, basis, []string{snap.CapsuleID, check.CapsuleID, events[i].CapsuleID}, false
	}
	if state.intent.MaxTotalMinor != nil {
		return state.intent.MaxTotalMinor, currency, "your limit", []string{events[0].CapsuleID}, true
	}
	if state.agreed.PriceMinor != nil {
		return state.agreed.PriceMinor, currency, "the agreed price", []string{events[0].CapsuleID}, false
	}
	return nil, "", "", nil, false
}

// merchantReport sets every sealed merchant email beside what was approved,
// and names the mismatches: charged more (or other) than approved, a likely
// duplicate charge, a charge dated before the email's own cancel-by date, and
// a quantity other than the one agreed. Each anomaly cites the steps it reads.
func merchantReport(events []sealedEvent, state dealState) ([]dealMerchantRow, []dealReportItem) {
	rows := []dealMerchantRow{}
	var anomalies []dealReportItem
	flag := func(kind, text string, steps ...string) {
		anomalies = append(anomalies, dealReportItem{Side: "counterparty", Kind: kind, Text: text, Steps: appendNew(nil, steps...)})
	}
	approved, approvedCur, basis, approvedSteps, limit := approvedAmount(events, state)
	var agentPaid []sealedEvent
	for _, se := range events {
		if a := se.Event.Act; a != nil && a.Action == "pay" && a.AmountMinor != nil {
			agentPaid = append(agentPaid, se)
		}
	}
	type charge struct {
		total    int64
		currency string
		order    string
		digest   string
		step     string
	}
	var charges []charge
	for _, se := range events {
		if se.Event.Evidence == nil || se.Event.Evidence.Email == nil {
			continue
		}
		m := se.Event.Evidence.Email
		p := m.Parsed
		row := dealMerchantRow{
			Steps: []string{se.CapsuleID}, OrderID: p.OrderID, Verified: m.DKIM.Result == "pass" && m.DKIM.Merchant,
			MerchantSays: emailVerdictWords(m.DKIM), WeSay: ourSealWords, CancelBy: p.CancelBy, KeySource: m.KeySource,
			SigningDomain: m.DKIM.signer(),
		}
		if m.DKIM.FromDomain != "" {
			row.MerchantApex = orgDomain(m.DKIM.FromDomain)
		}
		row.KeySize = m.keySize(row.SigningDomain)
		row.Domains = domainWords(row.SigningDomain, row.MerchantApex)
		unconfirmed := ""
		if !row.Verified {
			unconfirmed = " (this copy is not confirmed by the merchant's signature)"
		}
		if approved != nil {
			row.Approved, row.ApprovedBasis = formatMoney(*approved, approvedCur), basis
		}
		if len(agentPaid) > 0 {
			last := agentPaid[len(agentPaid)-1].Event.Act
			row.AgentReported = actText(*last, approvedCur)
		}
		if p.TotalMinor != nil {
			row.Charged = formatMoney(*p.TotalMinor, p.Currency)
			if len(p.SentAt) >= 10 {
				row.ChargedOn = p.SentAt[:10]
			}
			charges = append(charges, charge{*p.TotalMinor, p.Currency, p.OrderID, emailDigest(m.Raw), se.CapsuleID})
			sameCur := p.Currency == "" || approvedCur == "" || strings.EqualFold(p.Currency, approvedCur)
			switch {
			case approved == nil:
			case !sameCur:
				flag("charged_differs", fmt.Sprintf("Approved %s, but the merchant's email shows a charge in %s: %s%s", row.Approved, p.Currency, row.Charged, unconfirmed), append(approvedSteps, se.CapsuleID)...)
			case limit && *p.TotalMinor > *approved:
				flag("charged_differs", fmt.Sprintf("Your limit was %s; the merchant's email says %s%s", row.Approved, row.Charged, unconfirmed), append(approvedSteps, se.CapsuleID)...)
			case !limit && *p.TotalMinor != *approved:
				flag("charged_differs", fmt.Sprintf("Approved %s; the merchant's email says %s%s", row.Approved, row.Charged, unconfirmed), append(approvedSteps, se.CapsuleID)...)
			}
		}
		if p.ChargeAfterCancelBy && row.ChargedOn != "" && row.ChargedOn < p.CancelBy {
			flag("charged_before_cancel_by", fmt.Sprintf("Charged on %s, but the merchant's own email says nothing is charged before %s%s", row.ChargedOn, p.CancelBy, unconfirmed), se.CapsuleID)
		}
		var qty int64
		for _, it := range p.Items {
			row.Items = append(row.Items, fmt.Sprintf("%s × %d", it.Text, it.Quantity))
			qty += it.Quantity
		}
		if qty > 0 && state.agreed.Quantity > 0 && qty != state.agreed.Quantity {
			flag("quantity_differs", fmt.Sprintf("Agreed quantity %d; the merchant's email lists %d%s", state.agreed.Quantity, qty, unconfirmed), events[0].CapsuleID, se.CapsuleID)
		}
		rows = append(rows, row)
	}
	// A likely duplicate: the same amount charged under two different orders
	// (the same order confirmed twice is one charge), or paid twice by the agent.
	for i := range charges {
		for j := i + 1; j < len(charges); j++ {
			a, b := charges[i], charges[j]
			if a.total != b.total || !strings.EqualFold(a.currency, b.currency) || a.digest == b.digest || (a.order != "" && a.order == b.order) {
				continue
			}
			text := fmt.Sprintf("Possible duplicate charge: the merchant's emails show %s twice", formatMoney(a.total, a.currency))
			if a.order != "" && b.order != "" {
				text += " (orders " + a.order + " and " + b.order + ")"
			}
			flag("duplicate_charge", text, a.step, b.step)
		}
	}
	for i := range agentPaid {
		for j := i + 1; j < len(agentPaid); j++ {
			a, b := agentPaid[i].Event.Act, agentPaid[j].Event.Act
			if *a.AmountMinor == *b.AmountMinor && a.Currency == b.Currency {
				flag("duplicate_charge", "Possible duplicate charge: the agent reported paying "+actText(*a, approvedCur)+" twice", agentPaid[i].CapsuleID, agentPaid[j].CapsuleID)
			}
		}
	}
	return rows, anomalies
}
