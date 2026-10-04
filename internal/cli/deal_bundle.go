package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/spf13/cobra"
)

// `bundle --deal`, `disclose --deal` and `permalink --deal` read a deal's own
// log through the same builder as `deal report` (dealReportBundle: the whole
// deal, its cadence chain and any witness receipt held).
//
//   - bundle --deal writes the user's own copy: nothing withheld, nothing put
//     on record, the same file as `deal report --bundle`.
//   - disclose --deal and permalink --deal hand a copy to someone else, so
//     they are a share: --share counterparty|adjudicator and --to are
//     required, the copy withholds every record that audience may not see
//     (dealWithholdRecords), the final bytes pass the share gate
//     (dealPageGate) and the share is put on record (recordShare, on the
//     deal's disclosure log, deal/<deal_id>/disclosures) before anything is
//     written or printed.
func dealBundleRun(c *cobra.Command, use string) error {
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
			return inputError("bundle --deal writes your own copy, with nothing withheld; to hand a copy to someone, use disclose or permalink with --share counterparty|adjudicator --to WHO")
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
		report, err := s.dealReportFor(ctx, events)
		if err != nil {
			return err
		}
		b, err := s.dealReportBundle(ctx, events, report, audience, verify)
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
		// What is handed over: the bundle file, or the link. A link is only a
		// re-encoding of that gated bundle: its fragment is decoded back and
		// must be the same JCS bytes, so the link carries nothing the gate
		// did not read. (The gate is not run on the link itself: there the
		// allowed order id sits inside base64url, where it cannot be taken
		// out before the gate decodes the run and finds it.)
		shared := encoded
		if use == "permalink" {
			link, err := mintPermalink(c, b)
			if err != nil {
				return err
			}
			if err = sameBundle(link, b); err != nil {
				return err
			}
			shared = []byte(link)
		}
		share, err := s.recordShare(ctx, dealID, b, audience, recipient, shared)
		if err != nil {
			return err
		}
		if use == "permalink" {
			return output(c, map[string]any{"deal_id": dealID, "permalink": string(shared), "share": share})
		}
		if err = atomicFile(out, encoded, false); err != nil {
			return err
		}
		return output(c, map[string]any{"deal_id": dealID, "bundle": out, "share": share})
	})
}

// sameBundle checks that the link's fragment decodes to exactly the bundle,
// compared as JCS bytes.
func sameBundle(link string, b map[string]interface{}) error {
	_, fragment, ok := strings.Cut(link, "#")
	if !ok {
		return errors.New("permalink has no fragment")
	}
	decoded, err := aacbundle.DecodeFragment(fragment)
	if err != nil {
		return err
	}
	want, err := canonical.JCS(b)
	if err != nil {
		return err
	}
	got, err := canonical.JCS(decoded)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) {
		return errors.New("permalink fragment is not the gated bundle; nothing was put on record")
	}
	return nil
}

// mintPermalink encodes a bundle into a viewer link, refusing one too large
// to carry. A link carries the bundle in its fragment, which never leaves the
// reader's browser, so the hosted viewer holds nothing. A bundle too large for
// a link would have to be hosted by someone, which is custody; capsulectl does
// not do that.
func mintPermalink(c *cobra.Command, value map[string]interface{}) (string, error) {
	fragment, err := aacbundle.EncodeFragment(value)
	if err != nil {
		return "", err
	}
	decoded, err := aacbundle.DecodeFragment(fragment)
	if err != nil {
		return "", err
	}
	if _, ok := decoded.(map[string]interface{}); !ok {
		return "", errors.New("permalink fragment did not round-trip")
	}
	if limit, _ := c.Flags().GetInt("max-fragment"); limit > 0 && len(fragment) > limit {
		return "", inputError(fmt.Sprintf("too large for a link (%d characters; the limit is %d): share the bundle file instead (`capsulectl disclose ... --out FILE`)", len(fragment), limit))
	}
	base, _ := c.Flags().GetString("base-url")
	if base == "" {
		base = defaultBundleURL
	}
	return strings.TrimRight(base, "#") + "#" + fragment, nil
}
