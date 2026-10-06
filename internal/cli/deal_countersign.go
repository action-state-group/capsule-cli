package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Every deal receipt names its countersign rung. Today the honest value is
// "not countersigned": the receipt stands on the agent's own seal and any
// witness receipt. A countersignature is a signature by another party over
// one exact Evidence Bundle's digest (the countersign/v1 entries `countersign
// request` and `countersign verify` already handle). capsulectl never sends a
// deal's content anywhere to get one: `deal countersign` only verifies an
// entry over a bundle file this deal produced, and `deal report --from-bundle`
// renders the receipt from that same file, so the signature covers what the
// receipt shows. A countersignature by the producer's own key (a key in the
// profile's trusted_keys, or the one the bundle's producer-key/v1 extension
// declares) is shown as NOT INDEPENDENT, never as a countersignature.

// dealCountersignView is the countersign rung on the receipt, the report JSON
// and the page.
type dealCountersignView struct {
	// Rung is countersigned (a rung of the grade ladder, gradeLadder);
	// self_countersigned or unresolved_signer, annotations that are not rungs
	// and leave the grade where witnessing put it (gradeAnnotations); or
	// not_countersigned or unverified, which name no rung (gradeNoRung). The
	// strongest entry decides.
	Rung      string                   `json:"rung"`
	Text      string                   `json:"text"`
	Directory string                   `json:"directory,omitempty"`
	Entries   []countersignatureReport `json:"entries,omitempty"`
}

const dealNotCountersignedText = "Not countersigned: no other party has signed this record. It stands on the agent's own seal and any witness receipt above."

func dealNotCountersigned() dealCountersignView {
	return dealCountersignView{Rung: "not_countersigned", Text: dealNotCountersignedText}
}

// dealCountersignVerify verifies every countersignature the bundle carries,
// resolving signers in directory, and states the rung. An invalid entry (a
// signature or digest that does not verify) refuses: it is never rendered.
func dealCountersignVerify(ctx context.Context, b map[string]interface{}, directory string, p Profile) (dealCountersignView, error) {
	entries, _ := b["countersignatures"].([]interface{})
	if len(entries) == 0 {
		return dealNotCountersigned(), nil
	}
	if directory == "" {
		return dealCountersignView{}, inputError("the bundle carries countersignatures: --directory is required (a countersigner directory URL or file you choose) to resolve who signed")
	}
	trusted, err := parseKeys(p.TrustedKeys)
	if err != nil {
		return dealCountersignView{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	_, reports, summary, err := verifyCountersignatures(ctx, client, directory, b, trusted)
	if err != nil {
		return dealCountersignView{}, err
	}
	if err = hasInvalidCountersignature(reports); err != nil {
		return dealCountersignView{}, inputError("refusing to show a countersignature that does not verify: " + err.Error())
	}
	view := dealCountersignView{Directory: directory, Entries: reports}
	var lines []string
	for _, r := range reports {
		lines = append(lines, dealCountersignLine(r))
	}
	switch summary {
	case "resolved":
		view.Rung = "countersigned"
	case "not_independent":
		view.Rung = "self_countersigned"
	case "unresolved_signer":
		view.Rung = "unresolved_signer"
	case "unverified":
		view.Rung = "unverified"
	default:
		return dealNotCountersigned(), nil
	}
	view.Text = strings.Join(lines, " ")
	return view, nil
}

// dealCountersignLine is one countersignature in plain words.
func dealCountersignLine(r countersignatureReport) string {
	receipt := ""
	switch r.Receipt {
	case "verified":
		receipt = " Its log receipt verifies."
	case "absent":
		receipt = " It carries no log receipt."
	case "unverified":
		receipt = " Its log receipt does not verify."
	}
	switch r.State {
	case "resolved":
		var checks []string
		for _, c := range r.Checks {
			checks = append(checks, c.Name+": "+c.Result)
		}
		line := fmt.Sprintf("Countersigned by %s, a signer listed in the directory you chose: it signed this exact bundle", r.SignerName)
		if len(checks) > 0 {
			line += ", with its own results (" + strings.Join(checks, ", ") + ")"
		}
		return line + ". It vouches only as far as you trust that directory." + receipt
	case "not_independent":
		return "NOT INDEPENDENT: countersigned by the producer's own key. A self-countersignature is not a check by anyone else."
	case "unresolved_signer":
		return fmt.Sprintf("Countersigned by a key the directory you chose does not list (%s): its signer is unknown.", short(r.Signer.KeyID)) + receipt
	default:
		return "A countersignature of a type capsulectl does not check is attached; it is not counted."
	}
}

func short(keyHex string) string {
	if len(keyHex) > 16 {
		return keyHex[:16] + "…"
	}
	return keyHex
}

// dealBundleOf reads a bundle file and requires it to be this deal's own
// report bundle: it verifies, its log is the deal's, its checkpoint is signed
// by the profile's checkpoint key, its root is one of the deal's steps, and
// its x-deal-v0 extension names the deal.
func (s *dealSession) dealBundleOf(path, dealID string, events []sealedEvent) (map[string]interface{}, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	b, err := decodeBundleJSON(raw)
	if err != nil {
		return nil, err
	}
	notThis := func(why string) error {
		return inputError("--bundle " + path + " is not this deal's report bundle: " + why + " (write one with `deal report --deal " + dealID + " --bundle FILE`)")
	}
	if err = verifyProducedBundle(b, true); err != nil {
		return nil, notThis("it does not verify")
	}
	cert, _ := b["completeness_certificate"].(map[string]interface{})
	if fmt.Sprint(cert["log_id"]) != s.dp.LogID {
		return nil, notThis("its log is not this deal's")
	}
	cp, _ := b["checkpoint"].(map[string]interface{})
	statement, err := base64.StdEncoding.DecodeString(fmt.Sprint(cp["statement"]))
	if err != nil {
		return nil, notThis("its checkpoint is unreadable")
	}
	if _, err = verifyCheckpoint(s.dp, statement); err != nil {
		return nil, notThis("its checkpoint is not signed by this profile's checkpoint key")
	}
	root := fmt.Sprint(b["root"])
	found := false
	for _, se := range events {
		found = found || se.CapsuleID == root
	}
	if !found {
		return nil, notThis("its root is not a step of this deal")
	}
	ext, _ := b["extensions"].(map[string]interface{})
	deal, _ := ext["x-deal-v0"].(map[string]interface{})
	if deal["deal_id"] != dealID {
		return nil, notThis("its x-deal-v0 extension names another deal")
	}
	return b, nil
}

// decodeCountersignEntries reads one countersign/v1 entry, or an array of
// them, from a file.
func decodeCountersignEntries(path string) ([]CountersignatureEntry, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	var entries []CountersignatureEntry
	if strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(raw, &entries)
	} else {
		var one CountersignatureEntry
		err = json.Unmarshal(raw, &one)
		entries = []CountersignatureEntry{one}
	}
	if err != nil || len(entries) == 0 {
		return nil, inputError("--entry " + path + " must hold a countersign/v1 entry ({type, signer, over, statement, signature, receipt?}) or an array of them")
	}
	return entries, nil
}

func dealCountersignCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "countersign", Short: "Verify (and with --entry, attach) a countersignature on this deal's report bundle; sends nothing", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		bundlePath, _ := c.Flags().GetString("bundle")
		entryPath, _ := c.Flags().GetString("entry")
		directory, _ := c.Flags().GetString("directory")
		if bundlePath == "" {
			return inputError("--bundle is required: this deal's report bundle file, from `deal report --bundle`")
		}
		if directory == "" {
			return inputError("--directory is required: a countersigner directory URL or file you choose, to resolve who signed")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			b, err := s.dealBundleOf(bundlePath, dealID, events)
			if err != nil {
				return err
			}
			if entryPath != "" {
				entries, err := decodeCountersignEntries(entryPath)
				if err != nil {
					return err
				}
				if err = attachCountersignatures(b, entries); err != nil {
					return err
				}
			}
			view, err := dealCountersignVerify(ctx, b, directory, s.p)
			if err != nil {
				return err
			}
			out := map[string]any{"deal_id": dealID, "bundle": bundlePath, "countersign": view}
			if entryPath != "" {
				data, err := json.Marshal(b)
				if err != nil {
					return err
				}
				if err = atomicFile(bundlePath, data, true); err != nil {
					return err
				}
				out["attached"] = true
			}
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("bundle", "", "This deal's report bundle file (from `deal report --bundle`)")
	cmd.Flags().String("entry", "", "A countersign/v1 entry (or an array of them) to attach to the bundle once it verifies")
	cmd.Flags().String("directory", "", "Countersigner directory (HTTPS URL or file) that resolves signers; no list is privileged")
	return cmd
}
