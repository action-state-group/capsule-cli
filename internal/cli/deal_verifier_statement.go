package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"
)

// A verifier statement says, under the verifier's signature, that one sealed
// merchant email's DKIM signature checked out, without carrying the email:
// "domain D's DKIM signature verified over a message dated T, checked by V at
// T2". It names the email only by the digest the deal record already seals
// (message_digest) and the key records only by keyRecordsDigest, so it
// discloses no header, address, selector or signed-header list.
//
// It is issued only from a re-check of the full sealed raw message against the
// sealed key records, never from the verdict sealed earlier and never from a
// header set: what V signs is what V checked, now. A key the caller supplied
// is not what the merchant published, so no statement is issued over one.
//
// In v0 the verifier V is the holder's own signing key. The statement is
// therefore no more independent than the holder's own seal; what it adds is a
// smaller disclosure. Every place it is shown says "checked by the holder's
// own tool", never independent or third-party, and never a verified receipt.

// verifierStatementTag is the domain tag of a statement's signature input:
// UTF8(tag) || 0x00 || JCS(statement without "signature"). The holder key also
// signs capsules and countersignatures; the tag keeps a statement signature
// from ever verifying as either, and either from verifying as a statement.
const verifierStatementTag = "x-deal-v0/verifier-statement/v1"

// verifierStatementDef is the statement's definition in the x-deal-v0 schema.
const verifierStatementDef = "verifier_statement"

// verifierStatementType is the one statement_type v0 issues.
const verifierStatementType = "dkim-verification"

// verifierHolderWords names V for a v0 statement: the holder's own tool.
const verifierHolderWords = "checked by the holder's own tool"

// verifierStatementNoEmail is what verify-statement cannot do, said with
// every result.
const verifierStatementNoEmail = "DKIM itself is not re-checked here: the statement carries no email, only the verifier's signature over what it checked"

// ErrStatementInvalid is a verifier statement whose shape or signature does
// not check out. The result was printed.
var ErrStatementInvalid = errors.New("verifier statement did not verify")

var (
	statementSchemaOnce sync.Once
	statementSchema     *jsonschema.Schema
	statementSchemaErr  error
)

// compiledStatementSchema is the x-deal-v0 schema's verifier_statement
// definition, compiled on its own.
func compiledStatementSchema() (*jsonschema.Schema, error) {
	statementSchemaOnce.Do(func() { statementSchema, statementSchemaErr = compileDealSchemaAt("#/$defs/" + verifierStatementDef) })
	return statementSchema, statementSchemaErr
}

// validateStatement checks a statement, as decoded from JSON, against the
// schema.
func validateStatement(raw []byte) error {
	schema, err := compiledStatementSchema()
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return schema.Validate(doc)
}

// statementSigningInput is UTF8(tag) || 0x00 || JCS(statement without
// "signature").
func statementSigningInput(statement map[string]interface{}) ([]byte, error) {
	unsigned := make(map[string]interface{}, len(statement))
	for k, v := range statement {
		if k != "signature" {
			unsigned[k] = v
		}
	}
	jcs, err := canonical.JCS(unsigned)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(verifierStatementTag), 0x00), jcs...), nil
}

// issueVerifierStatement re-checks the sealed email, in full, against its
// sealed key records and, only when a signature passes, returns the statement
// signed by key at now. The domain is the signer the verdict rests on (the
// merchant-aligned one when there is one).
func issueVerifierStatement(e *merchantEmail, key ed25519.PrivateKey, now time.Time) (map[string]interface{}, error) {
	if e.KeySource != "dns" {
		return nil, inputError("no statement: this email's key record was supplied by hand, not read from the merchant's DNS, so a statement would present it as the merchant's key")
	}
	v, err := e.recheck(e.Raw)
	if err != nil {
		return nil, err
	}
	domain := v.signer()
	if v.Result != "pass" || domain == "" {
		return nil, inputError("no statement: the email's DKIM signature does not check out against the key records sealed with it (" + v.Result + ")")
	}
	var signed []string
	for _, s := range v.Signatures {
		if s.Result == "pass" && s.Domain == domain {
			signed = s.Signed
			break
		}
	}
	keys, err := keyRecordsDigest(e.Keys)
	if err != nil {
		return nil, err
	}
	public := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	statement := map[string]interface{}{
		"statement_type":     verifierStatementType,
		"dkim_domain":        domain,
		"email_digest":       map[string]interface{}{"alg": "sha-256", "value": emailDigest(e.Raw)},
		"key_records_digest": keys,
		"key_source":         e.KeySource,
		"checked_at":         now.UTC().Format("2006-01-02T15:04:05Z"),
		"verifier":           map[string]interface{}{"key_id": public, "public_key": public},
	}
	// A Date the passing signature does not cover is the sender's (or the
	// holder's) unverified claim: it is left out, never stated. A covered Date
	// is stated only when it is the message's one Date header: DKIM signs the
	// bottom-most instance of a header, while a reader takes the first, so an
	// unsigned Date added above the signed one would otherwise be stated as
	// covered. A covered Date that is repeated or unreadable gets no statement,
	// since "date not covered" would then be untrue.
	if dateSigned(signed) {
		date, err := signedDate(e.Raw)
		if err != nil {
			return nil, err
		}
		statement["message_date"] = date
	}
	input, err := statementSigningInput(statement)
	if err != nil {
		return nil, err
	}
	statement["signature"] = hex.EncodeToString(ed25519.Sign(key, input))
	return statement, nil
}

// dateSigned is whether a signature's h= list covers the Date header.
func dateSigned(headers []string) bool {
	for _, h := range headers {
		if strings.EqualFold(strings.TrimSpace(h), "Date") {
			return true
		}
	}
	return false
}

// signedDate is the message's one Date header in UTC, for a signature whose
// h= covers Date.
func signedDate(raw []byte) (string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", inputError("no statement: the sealed email's headers cannot be read")
	}
	if len(msg.Header["Date"]) != 1 {
		return "", inputError("no statement: the signature covers the Date header, but the email has more than one, so which date it signed cannot be stated")
	}
	t, err := msg.Header.Date()
	if err != nil {
		return "", inputError("no statement: the signature covers the Date header, but its date cannot be read")
	}
	return t.UTC().Format("2006-01-02T15:04:05Z"), nil
}

// verifyStatement checks a statement file's shape and its signature (with the
// domain tag) under the key it names, and returns the statement decoded.
func verifyStatement(raw []byte) (map[string]interface{}, error) {
	if err := validateStatement(raw); err != nil {
		return nil, errors.Join(errors.New("the statement is not an x-deal-v0 verifier_statement"), err)
	}
	statement, err := decodeBundleJSON(raw)
	if err != nil {
		return nil, err
	}
	verifier, _ := statement["verifier"].(map[string]interface{})
	publicHex, _ := verifier["public_key"].(string)
	if verifier["key_id"] != publicHex {
		return nil, errors.New("the verifier's key_id is not its public key")
	}
	public, _ := hex.DecodeString(publicHex)
	signature, _ := hex.DecodeString(statement["signature"].(string))
	input, err := statementSigningInput(statement)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(ed25519.PublicKey(public), input, signature) {
		return nil, errors.New("the signature does not verify under the verifier's key")
	}
	return statement, nil
}

// statementWords is what a statement says, as V's statement and nothing more.
func statementWords(statement map[string]interface{}) string {
	date := "a message (date not covered by the signature)"
	if t, ok := statement["message_date"].(string); ok {
		date = "a message dated " + t
	}
	key, _ := statement["verifier"].(map[string]interface{})["key_id"].(string)
	return "V's statement: DKIM signature from " + statement["dkim_domain"].(string) + " verified over " + date +
		", " + verifierHolderWords + " (key " + key + ") at " + statement["checked_at"].(string) + "."
}

func dealVerifierStatementCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "verifier-statement", Short: "Write a signed statement that a sealed merchant email's DKIM signature checked out, re-checked now over the full sealed email; it carries no email, and is checked by the holder's own tool", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		step, _ := c.Flags().GetInt64("step")
		out, _ := c.Flags().GetString("out")
		if out == "" {
			return inputError("--out is required: the new statement file")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			var found []sealedEvent
			for _, se := range events {
				if m := se.Event.Evidence; se.Event.Kind == "evidence" && m != nil && m.Email != nil && (step == 0 || se.Event.N == step) {
					found = append(found, se)
				}
			}
			switch {
			case len(found) == 0:
				return inputError("no sealed merchant email in this deal (or at that step)")
			case len(found) > 1:
				return inputError("this deal has more than one sealed merchant email: name one with --step")
			}
			se := found[0]
			statement, err := issueVerifierStatement(se.Event.Evidence.Email, s.key, dealClock())
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(statement)
			if err != nil {
				return err
			}
			if err = validateStatement(encoded); err != nil {
				return err
			}
			if err = atomicFile(out, encoded, false); err != nil {
				return err
			}
			return output(c, map[string]any{"deal_id": dealID, "step": se.Event.N, "out": out, "says": statementWords(statement), "verifier": verifierHolderWords})
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().Int64("step", 0, "The evidence step holding the email (needed when the deal has more than one)")
	cmd.Flags().String("out", "", "New file for the statement")
	return cmd
}

// verifierKeyUnmatched is said when verify-statement was given no key to
// match the statement's against: the statement names its own key, so without
// --key nothing ties that key to the holder.
const verifierKeyUnmatched = "the key is the one the statement names; it is not matched to any holder here (pass --key with the holder's public key to check it)"

func dealVerifyStatementCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "verify-statement FILE", Short: "Check a verifier statement's signature offline and say what it states, as V's statement; DKIM itself is not re-checked (the statement carries no email), and the key is matched to the holder only with --key", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		want, _ := c.Flags().GetString("key")
		raw, err := readInput(args[0])
		if err != nil {
			return err
		}
		result := map[string]any{"dkim_rechecked": false, "note": verifierStatementNoEmail, "key": verifierKeyUnmatched}
		statement, err := verifyStatement(raw)
		if err == nil && want != "" {
			if statement["verifier"].(map[string]interface{})["key_id"] != strings.ToLower(want) {
				err = errors.New("the statement is signed by another key than --key")
			} else {
				result["key"] = "matches --key"
			}
		}
		if err != nil {
			result["verified"], result["reason"] = false, err.Error()
			if oerr := output(c, result); oerr != nil {
				return oerr
			}
			return ErrStatementInvalid
		}
		result["verified"], result["says"] = true, statementWords(statement)
		return output(c, result)
	}}
	cmd.Flags().String("key", "", "The holder's public key (64 hex); the statement must be signed by it")
	return cmd
}
