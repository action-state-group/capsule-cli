package cli

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/spf13/cobra"
)

// `bundle --deal` and `disclose --deal` read a deal's own log through the
// same builder as `deal report` (dealReportBundle: the whole deal, its
// cadence chain and any witness receipt held).
//
//   - bundle --deal writes the user's own copy: nothing withheld, nothing put
//     on record, the same file as `deal report --bundle`.
//   - disclose --deal hands a copy to someone else, so it is a share:
//     --share counterparty|adjudicator and --to are required, the copy
//     withholds every record that audience may not see
//     (dealWithholdRecords), the final bytes pass the share gate and the
//     share is put on record (recordShare, on the deal's disclosure log,
//     deal/<deal_id>/disclosures) before the file is written.
//
// A deal is not shared as a link: the user hands over the report file, and
// a reader who would rather not take the page's word drops it into a
// verifier.
func dealBundleRun(c *cobra.Command, use string) error {
	if use == "permalink" {
		return inputError("a deal is not shared as a link: hand over the report file instead, `deal report --deal ID --html FILE` or `--email FILE` (your own copy), or `deal report --deal ID --share counterparty|adjudicator --to WHO --html FILE` (a copy for someone else)")
	}
	if c.Flags().Changed("root") || c.Flags().Changed("closure-depth") || c.Flags().Changed("log-id") {
		return inputError("--deal takes the whole deal from its own log: --root, --closure-depth and --log-id do not apply")
	}
	if c.Flags().Changed("payloads") || c.Flags().Changed("suppress") {
		return inputError("--deal discloses each step's x-deal-v0 record as the audience allows (--share): --payloads and --suppress do not apply")
	}
	audience, _ := c.Flags().GetString("share")
	recipient, _ := c.Flags().GetString("to")
	recipient = strings.TrimSpace(recipient)
	switch use {
	case "bundle":
		if audience != dealAudienceKeep || recipient != "" {
			return inputError("bundle --deal writes your own copy, with nothing withheld; to hand a copy to someone, use disclose --deal with --share counterparty|adjudicator --to WHO, or `deal report --share`")
		}
	default:
		if audience != dealAudienceCounterparty && audience != dealAudienceAdjudicator {
			return inputError(use + " --deal hands a copy to someone else: it needs --share counterparty or adjudicator (what that reader may see) and --to WHO; your own copy is `bundle --deal`")
		}
		if recipient == "" {
			return inputError(use + " --deal needs --to: who the shared copy is for, as sealed in the disclosure record")
		}
	}
	out, _ := c.Flags().GetString("out")
	if use == "disclose" && out == "" {
		return inputError("disclose --deal needs --out FILE: the shared bundle is written there after the share is on record")
	}
	return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
		verify := dealVerifyCommand("bundle.json")
		if out != "" {
			verify = dealVerifyCommand(out)
		}
		b, err := s.dealReportBundle(ctx, events, buildDealReport(events), audience, verify)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if use == "bundle" {
			if out != "" {
				return atomicFile(out, encoded, false)
			}
			_, err = c.OutOrStdout().Write(append(encoded, '\n'))
			return err
		}
		// The gate reads the shared bundle's JSON, with the audience's
		// allow-list, before anything is on record or written.
		if err = dealShareGate(encoded, events, audience); err != nil {
			return err
		}
		share, err := s.recordShare(ctx, dealID, b, audience, recipient, encoded)
		if err != nil {
			return err
		}
		if err = atomicFile(out, encoded, false); err != nil {
			return err
		}
		return output(c, map[string]any{"deal_id": dealID, "bundle": out, "share": share})
	})
}
