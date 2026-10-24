package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	aacsettlement "github.com/action-state-group/agent-action-capsule/go/settlement"
	"github.com/action-state-group/capsule-cli/cli"
	"github.com/spf13/cobra"
)

// A held object on a buyer's deal (capsule-cli#250; design note on the
// issue, v2 with its v3 and v3.1 amendments). Some objects a buyer's agent
// receives from the other side, a checkout response or a receipt, prove
// nothing by being restated: what can be sealed is that the user holds these
// exact octets (settlement-records-00 §8, "Wrapping Existing Signed
// Objects"). The step's record carries one wrapped entry in that section's
// shape, {type, digest_alg, digest}, never its content; the octets and the
// carrier headers they arrived with stay in the step's local event, which a
// book deal keeps in the artifact store as its deal_local opening. Nothing is
// read from the object: no amount, id or name enters the record, and the
// object's own signature, if it has one, is its protocol's to check.

// Which octets a wrapped digest is over (SR §8 rule 1, in the payload-binding
// draft's terms): the octets as delivered, or the RFC 8785 form of the object
// where its type selects it.
const (
	wrappedAsTransmitted = "as-transmitted"
	wrappedJCS           = "jcs"
)

// wrappedHeldOctets and wrappedHeldDigest say what the sealer held: the
// object, or only a digest someone gave it, which claims no possession.
const (
	wrappedHeldOctets = "octets"
	wrappedHeldDigest = "digest"
)

// wrappedSource is a held object's evidence source: a fixed token, never a
// caller's word.
const wrappedSource = "wrapped_object"

const (
	// maxWrappedObject keeps a step's artifact record (its Capsule,
	// envelope, payload and the deal_local opening, where the object is
	// base64) within the artifact store's 8 MiB record limit.
	maxWrappedObject = 4 << 20
	// maxCarrierHeaders bounds a header block.
	maxCarrierHeaders = 64 << 10
)

// wrappedByForm marks a registered type whose octets depend on the object's
// form: SR's x402.offer and x402.receipt are the JWS compact serialization's
// ASCII octets, or the RFC 8785 form of the EIP-712 envelope.
const wrappedByForm = "by-form"

// wrappedOctetsRules is the octets rule of every type in SR's initial
// registry ("Wrapped Object Types"), keyed exactly as
// agent-action-capsule's settlement.WrappedTypes: a test holds the two key
// sets equal, so a type the module registers later fails until its rule is
// added here.
var wrappedOctetsRules = map[string]string{
	"x402.offer":           wrappedByForm,
	"x402.receipt":         wrappedByForm,
	"x402.payment-payload": wrappedAsTransmitted,
	"x402.settle-response": wrappedAsTransmitted,
	"ap2.checkout-mandate": wrappedAsTransmitted,
	"ap2.payment-mandate":  wrappedAsTransmitted,
	"ap2.checkout-receipt": wrappedAsTransmitted,
	"ap2.payment-receipt":  wrappedAsTransmitted,
	"bolt12.invoice":       wrappedAsTransmitted,
	"bolt12.payer-proof":   wrappedAsTransmitted,
	"mpp.payment-receipt":  wrappedAsTransmitted,
	"iso20022.message":     wrappedAsTransmitted,
	"delivery.proof":       wrappedAsTransmitted,
}

// wrappedTypePattern is a type name: the caller's label when it is not
// registered.
var wrappedTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

// jwsCompact is a JWS compact serialization: three base64url segments.
var jwsCompact = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+$`)

// dealWrapped is a held object as the step's local event keeps it. Object is
// absent when only the digest was given.
type dealWrapped struct {
	Type    string `json:"type"`
	Octets  string `json:"octets"`
	Digest  string `json:"digest"`
	Object  []byte `json:"object,omitempty"`
	Headers []byte `json:"headers,omitempty"`
}

// wrappedAbout is a held object note's default about: a digest given
// without the object claims no possession.
func wrappedAbout(w *dealWrapped) string {
	if w.Object == nil {
		return "held digest"
	}
	return "held object"
}

func (w *dealWrapped) held() string {
	if w.Object != nil {
		return wrappedHeldOctets
	}
	return wrappedHeldDigest
}

// wrappedTypeRegistered is whether a type is in SR's registry. Any other
// name is the sealer's label for an opaque digest (acp-v0 open point A-3's
// interim), never presented as registered.
func wrappedTypeRegistered(kind string) bool {
	_, ok := aacsettlement.WrappedTypes[kind]
	return ok
}

// wrappedDigest is the SHA-256 of an object's octets under a rule.
func wrappedDigest(object []byte, rule string) (string, error) {
	data := object
	if rule == wrappedJCS {
		form, err := jcsForm(object)
		if err != nil {
			return "", err
		}
		data = form
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// jcsForm is the RFC 8785 form of an object read as I-JSON (RFC 7493):
// invalid UTF-8, a lone surrogate escape, a duplicate member or trailing
// data makes it unreadable, and so does a number outside binary64's range,
// which RFC 8785 cannot write (Canonicalize would clamp it to the largest
// finite value, giving two objects one form). The form is the standard
// library's (jsontext.Value.Canonicalize, RFC 8785 with ECMAScript number
// output), since a foreign object's numbers need not be integers.
func jcsForm(object []byte) ([]byte, error) {
	if err := canonical.CheckJSONText(object); err != nil {
		return nil, errors.New("it is not I-JSON: " + err.Error())
	}
	dec := jsontext.NewDecoder(bytes.NewReader(object))
	for {
		tok, err := dec.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("it is not I-JSON (a syntax error, a duplicate member or trailing data)")
		}
		if tok.Kind() == '0' {
			if _, err := strconv.ParseFloat(tok.String(), 64); err != nil {
				return nil, errors.New("it is not I-JSON: the number " + tok.String() + " is outside binary64's range")
			}
		}
	}
	form := jsontext.Value(bytes.Clone(object))
	if err := form.Canonicalize(); err != nil {
		return nil, errors.New("it is not I-JSON (a syntax error, a duplicate member or trailing data)")
	}
	return form, nil
}

// wrappedFormRule is the rule an x402 offer or receipt's held octets select:
// the EIP-712 envelope (exactly format, payload and signature) is jcs, a JWS
// compact serialization as-transmitted, and anything else neither.
func wrappedFormRule(object []byte) (string, error) {
	if jwsCompact.Match(object) {
		return wrappedAsTransmitted, nil
	}
	if trimmed := bytes.TrimSpace(object); len(trimmed) != len(object) && jwsCompact.Match(trimmed) {
		return "", errors.New("it is a JWS compact serialization with trailing whitespace: seal the octets exactly as received, without the added newline or spaces")
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(object, &members) == nil && len(members) == 3 {
		_, f := members["format"]
		_, p := members["payload"]
		_, s := members["signature"]
		if f && p && s {
			return wrappedJCS, nil
		}
	}
	return "", errors.New("it is neither a JWS compact serialization nor an EIP-712 envelope (exactly format, payload and signature), the two forms the type defines")
}

// checkWrappedRule holds a registered type to its registry rule. An
// unregistered type's rule is the caller's statement, sealed beside it.
func checkWrappedRule(w *dealWrapped) error {
	rule, registered := wrappedOctetsRules[w.Type]
	switch {
	case !registered || rule == w.Octets:
		return nil
	case rule == wrappedByForm && w.Object == nil:
		// Only a digest: there are no octets to read the form from, and
		// wrapped_held says the rule is the sealer's statement.
		return nil
	case rule == wrappedByForm:
		form, err := wrappedFormRule(w.Object)
		if err != nil {
			return cli.InputError("--object is not a " + w.Type + ": " + err.Error())
		}
		if form != w.Octets {
			return cli.InputError("--octets " + w.Octets + " does not fit this " + w.Type + ": its form takes " + form)
		}
		return nil
	default:
		return cli.InputError("a " + w.Type + " digest is over its octets as delivered: --octets " + rule)
	}
}

// wrappedNoteInput reads a held object from the note's flags, or nil when
// none was named.
func wrappedNoteInput(c *cobra.Command) (*dealWrapped, error) {
	flag := func(name string) string { v, _ := c.Flags().GetString(name); return v }
	objectPath, digest, kind, rule, headersPath := flag("object"), strings.TrimSpace(flag("object-digest")), flag("object-type"), flag("octets"), flag("headers")
	if objectPath == "" && digest == "" {
		if kind != "" || c.Flags().Changed("octets") || headersPath != "" {
			return nil, cli.InputError("--object-type, --octets and --headers go with --object or --object-digest")
		}
		return nil, nil
	}
	switch {
	case objectPath != "" && digest != "":
		return nil, cli.InputError("--object and --object-digest are alternatives: the object's octets, or only its SHA-256")
	case flag("email") != "":
		return nil, cli.InputError("--email and a held object are separate notes")
	case kind == "":
		return nil, cli.InputError("--object-type is required: the wrapped object's type (a settlement-records registered type, such as delivery.proof, or your own label)")
	case !wrappedTypePattern.MatchString(kind):
		return nil, cli.InputError("--object-type is lowercase letters, digits, . and - (at most 64)")
	case rule != wrappedAsTransmitted && rule != wrappedJCS:
		return nil, cli.InputError("--octets is as-transmitted (the octets as delivered) or jcs (their RFC 8785 form)")
	}
	w := &dealWrapped{Type: kind, Octets: rule}
	if objectPath != "" {
		object, err := readCapped(objectPath, "--object", maxWrappedObject)
		if err != nil {
			return nil, err
		}
		if len(object) == 0 {
			return nil, cli.InputError("--object " + objectPath + " is empty")
		}
		w.Object = object
	} else {
		if len(digest) != 64 || !cli.IsLowerHex(digest) {
			return nil, cli.InputError("--object-digest is a SHA-256: 64 lowercase hex characters")
		}
		w.Digest = digest
	}
	if headersPath != "" {
		headers, err := readCapped(headersPath, "--headers", maxCarrierHeaders)
		if err != nil {
			return nil, err
		}
		if len(headers) == 0 || bytes.IndexByte(headers, 0) >= 0 {
			return nil, cli.InputError("--headers is the start line and header fields as received: not empty, and no NUL")
		}
		w.Headers = headers
	}
	if err := checkWrappedRule(w); err != nil {
		return nil, err
	}
	if w.Object != nil {
		var err error
		if w.Digest, err = wrappedDigest(w.Object, rule); err != nil {
			return nil, cli.InputError("--octets jcs: --object " + err.Error())
		}
	}
	return w, nil
}

func readCapped(path, name string, limit int) ([]byte, error) {
	data, err := cli.ReadInput(path)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, cli.InputError(fmt.Sprintf("%s %s is over %d bytes", name, path, limit))
	}
	return data, nil
}

// headersText is the text a carrier_headers commitment binds: the exact
// header bytes, base64url without padding, since header values need not be
// UTF-8 and a commitment is over text.
func headersText(headers []byte) string {
	return base64.RawURLEncoding.EncodeToString(headers)
}

// wrappedBody adds a held object's members to an evidence record's body.
func wrappedBody(body map[string]interface{}, w *dealWrapped, commit func(string) (string, error)) error {
	body["wrapped"] = []interface{}{map[string]interface{}{"type": w.Type, "digest_alg": "SHA-256", "digest": w.Digest}}
	body["wrapped_octets"] = w.Octets
	body["wrapped_held"] = w.held()
	if w.Headers != nil {
		c, err := commit("carrier_headers")
		if err != nil {
			return err
		}
		body["carrier_headers_commitment"] = c
	}
	return nil
}

// checkWrappedNote holds a held object to the deal it is sealed on: a buyer's
// deal kept on a book.
func checkWrappedNote(s *dealSession, events []sealedEvent) error {
	if !s.onBook {
		return cli.InputError("a held object is sealed on a deal kept on a book; this deal is an older one, which takes no new kinds of step: open a new deal (`deal open`)")
	}
	if dealRole(events) == dealRoleSeller {
		return cli.InputError("a held object is recorded on a deal where the user buys; where the user sells, payment_received and delivered wrap the rail's objects")
	}
	return nil
}

// headerValues are the values of a held object's header lines, trimmed and
// valid UTF-8. The start line has none.
func headerValues(w *dealWrapped) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(w.Headers), "\r\n", "\n"), "\n") {
		if _, value, ok := strings.Cut(line, ":"); ok && utf8.ValidString(value) && strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

// wrappedHeaderValues are the header values of eight bytes or more, as exact
// private values for the share gate (a token or a request id need not look
// like one).
func wrappedHeaderValues(w *dealWrapped) []string {
	return slices.DeleteFunc(headerValues(w), func(v string) bool { return len(v) < 8 })
}

// wrappedTexts are a held object's private texts for the share gate: the
// object, when it is text, and its header values. The gate mines them for
// identifiers as it mines a sealed email's text; capsule-cli does not read
// the object's fields. A JSON number value in the object equal to an amount
// the deal's copies show anyway (shown, in minor units) is not mined: a held
// checkout states the deal's own total, and that alone must not refuse every
// copy. Digits inside a string (an id such as "ref-5400-abc") are always
// mined.
func wrappedTexts(w *dealWrapped, shown map[int64]bool) []string {
	var out []string
	if w.Object != nil && utf8.Valid(w.Object) {
		out = append(out, string(blankShownNumbers(w.Object, shown)))
	}
	return append(out, headerValues(w)...)
}

// blankShownNumbers blanks the JSON number values of object that are shown
// amounts. An object that is not exactly one JSON value (a decoder alone
// would take a stream of several) is returned unchanged.
func blankShownNumbers(object []byte, shown map[int64]bool) []byte {
	opts := []jsontext.Options{jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)}
	if !jsontext.Value(object).IsValid(opts...) {
		return object
	}
	out := bytes.Clone(object)
	dec := jsontext.NewDecoder(bytes.NewReader(object), opts...)
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			if err != io.EOF {
				return object
			}
			return out
		}
		if tok.Kind() != '0' {
			continue
		}
		raw := tok.String()
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && shown[v] {
			end := int(dec.InputOffset())
			copy(out[end-len(raw):end], strings.Repeat(" ", len(raw)))
		}
	}
}

// wrappedShownAmounts are the minor-unit amounts a deal's copies show in the
// clear: what was asked, approved and paid, and a received close's price.
// Never the user's limit or floor, which the ceiling gate keeps.
func wrappedShownAmounts(events []sealedEvent) map[int64]bool {
	shown := dealShownAmounts(events)
	for _, se := range events {
		if c := se.Event.Close; c != nil && c.Delivered != nil && c.Delivered.PriceMinor != nil {
			shown[*c.Delivered.PriceMinor] = true
		}
	}
	return shown
}

// wrappedOutput is what a sealed held object reports.
func wrappedOutput(out map[string]any, w *dealWrapped) {
	out["wrapped"] = map[string]any{"type": w.Type, "type_registered": wrappedTypeRegistered(w.Type), "digest": w.Digest,
		"wrapped_octets": w.Octets, "wrapped_held": w.held(), "carrier_headers": w.Headers != nil}
	out["scope"] = wrappedScopeLine
}

const wrappedScopeLine = "possession of these exact octets; the object's own signature, if it has one, is its protocol's to check (settlement-records §8 rule 4)"

// Verify statuses, in the evidence vocabulary.
const (
	verifyPass       = "PASS"
	verifyFail       = "FAIL"
	verifyNotPresent = "NOT_PRESENT"
)

func dealVerifyObjectCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "verify-object", Short: "Check a held object against the digest its deal step sealed, or any object against a digest", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		flag := func(name string) string { v, _ := c.Flags().GetString(name); return v }
		read := func(name string, limit int) ([]byte, error) {
			if flag(name) == "" {
				return nil, nil
			}
			return readCapped(flag(name), "--"+name, limit)
		}
		object, err := read("object", maxWrappedObject)
		if err != nil {
			return err
		}
		headers, err := read("headers", maxCarrierHeaders)
		if err != nil {
			return err
		}
		if flag("deal") == "" {
			if headers != nil || flag("out") != "" || c.Flags().Changed("step") {
				return cli.InputError("--step, --headers and --out go with --deal: without it, verify-object checks --object against --digest")
			}
			return verifyObjectAlone(c, object, flag("digest"), flag("octets"))
		}
		if flag("digest") != "" || c.Flags().Changed("octets") {
			return cli.InputError("--digest and --octets are for checking an object without a deal; with --deal they are the sealed ones")
		}
		step, _ := c.Flags().GetInt64("step")
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			var results []map[string]any
			var held []*dealWrapped
			ok := true
			for _, se := range events {
				e := se.Event.Evidence
				if e == nil || e.Wrapped == nil || (step != 0 && se.Event.N != step) {
					continue
				}
				r, pass, err := verifyWrapped(se, e.Wrapped, object, headers)
				if err != nil {
					return err
				}
				ok = ok && pass
				results = append(results, r)
				held = append(held, e.Wrapped)
			}
			if len(results) == 0 {
				return cli.InputError("no held object in this deal (or at that step)")
			}
			if len(results) > 1 && (object != nil || headers != nil) {
				return cli.InputError("--object and --headers are a copy of one held object: name its step with --step")
			}
			if dir := flag("out"); dir != "" {
				if len(held) != 1 {
					return cli.InputError("--out writes one held object: name its step with --step")
				}
				if err := writeHeld(dir, held[0]); err != nil {
					return err
				}
			}
			if err := cli.Output(c, map[string]any{"deal_id": dealID, "scope": wrappedScopeLine, "matches": ok, "objects": results}); err != nil {
				return err
			}
			if !ok {
				return cli.ErrObjectUnverified
			}
			return nil
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`; without it, checks --object against --digest")
	cmd.Flags().Int64("step", 0, "Only the held object sealed at this step")
	cmd.Flags().String("object", "", "Check this copy of the object instead of the held octets")
	cmd.Flags().String("headers", "", "Check this copy of the carrier headers instead of the held ones")
	cmd.Flags().String("out", "", "Write the held object and headers, exactly, as object and headers in this existing directory (one step)")
	cmd.Flags().String("digest", "", "Without --deal: the SHA-256 a sealed record states for the object (its wrapped digest)")
	cmd.Flags().String("octets", wrappedAsTransmitted, "Without --deal: what the digest is over, as the record's wrapped_octets says: as-transmitted or jcs")
	return cmd
}

// verifyWrapped checks one sealed held object: its octets (held, or a copy)
// against the sealed digest, and its headers (held, or a copy) against their
// commitment. The step loaded, so the held bytes are the sealed ones.
func verifyWrapped(se sealedEvent, w *dealWrapped, object, headers []byte) (map[string]any, bool, error) {
	r := map[string]any{"step": se.Event.N, "capsule_id": se.CapsuleID, "type": w.Type, "type_registered": wrappedTypeRegistered(w.Type),
		"wrapped_held": w.held(), "wrapped_octets": w.Octets, "digest": w.Digest}
	ok := true
	source := "store"
	if object != nil {
		source = "file"
	} else {
		object = w.Object
	}
	switch {
	case object == nil:
		r["object_digest"] = verifyNotPresent
		source = "none"
	default:
		got, err := wrappedDigest(object, w.Octets)
		if err == nil && got == w.Digest {
			r["object_digest"] = verifyPass
		} else {
			r["object_digest"], r["code"] = verifyFail, "wrapped_digest_mismatch"
			ok = false
		}
	}
	r["object_source"] = source
	switch {
	case w.Headers == nil:
		r["carrier_headers"] = verifyNotPresent
		if headers != nil {
			r["headers_given"] = "not checked: this step sealed no carrier headers"
		}
	default:
		// The load re-derived the record, so the held headers open its
		// commitment; a copy is checked against them byte for byte.
		if headers == nil || bytes.Equal(headers, w.Headers) {
			r["carrier_headers"] = verifyPass
		} else {
			r["carrier_headers"] = verifyFail
			ok = false
		}
	}
	return r, ok, nil
}

// verifyObjectAlone checks an object against a digest from a copy's record,
// with no deal: the recipient's check.
func verifyObjectAlone(c *cobra.Command, object []byte, digest, rule string) error {
	switch {
	case object == nil || digest == "":
		return cli.InputError("without --deal, verify-object checks --object against --digest (the wrapped digest in the record you were given)")
	case len(digest) != 64 || !cli.IsLowerHex(digest):
		return cli.InputError("--digest is a SHA-256: 64 lowercase hex characters")
	case rule != wrappedAsTransmitted && rule != wrappedJCS:
		return cli.InputError("--octets is as-transmitted or jcs, as the record's wrapped_octets says")
	}
	r := map[string]any{"digest": digest, "wrapped_octets": rule, "scope": wrappedScopeLine}
	got, err := wrappedDigest(object, rule)
	ok := err == nil && got == digest
	if ok {
		r["object_digest"] = verifyPass
	} else {
		r["object_digest"], r["code"] = verifyFail, "wrapped_digest_mismatch"
	}
	r["matches"] = ok
	if err := cli.Output(c, r); err != nil {
		return err
	}
	if !ok {
		return cli.ErrObjectUnverified
	}
	return nil
}

// writeHeld writes a held object and its headers, exactly, into an existing
// directory, never over a file already there.
func writeHeld(dir string, w *dealWrapped) error {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return cli.InputError("--out " + dir + " is not an existing directory")
	}
	if w.Object == nil {
		return cli.InputError("this step holds only a digest: there is no object to write")
	}
	type file struct {
		name string
		data []byte
	}
	files := []file{{"object", w.Object}}
	if w.Headers != nil {
		files = append(files, file{"headers", w.Headers})
	}
	// Both names are checked before either is written, so a refusal leaves
	// nothing behind.
	for _, f := range files {
		if _, err := os.Lstat(filepath.Join(dir, f.name)); !errors.Is(err, os.ErrNotExist) {
			return cli.InputError("--out: " + filepath.Join(dir, f.name) + " already exists")
		}
	}
	for _, f := range files {
		out, err := os.OpenFile(filepath.Join(dir, f.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return cli.InputError("--out: cannot create " + filepath.Join(dir, f.name))
		}
		_, err = out.Write(f.data)
		if err = errors.Join(err, out.Close()); err != nil {
			return err
		}
	}
	return nil
}
