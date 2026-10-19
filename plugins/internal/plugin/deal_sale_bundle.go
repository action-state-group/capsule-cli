package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/spf13/cobra"
)

func dealSaleBundleCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "bundle", Short: "Write your own copy of a sale: its log, every registered thread, and proof the list of threads is whole", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		saleID, _ := c.Flags().GetString("sale")
		out, _ := c.Flags().GetString("out")
		if !cli.SaleIDPattern.MatchString(saleID) || out == "" {
			return cli.InputError("--sale (a sale id as printed by `deal sale new`) and --out are required")
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			b, summary, err := s.dealSaleBundle(ctx, saleID, dealVerifyCommand(out))
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(b)
			if err != nil {
				return err
			}
			if err = cli.AtomicFile(out, encoded, false); err != nil {
				return err
			}
			summary["sale_id"], summary["out"] = saleID, out
			return cli.Output(c, summary)
		})
	}}
	cmd.Flags().String("sale", "", "Sale ID from `deal sale new`")
	cmd.Flags().String("out", "", "New file for the sale bundle")
	return cmd
}

// dealSaleBundle builds the user's own sale bundle: the sale log's Evidence
// Bundle through a checkpoint cut now, so its completeness certificate
// covers every registration so far; each registered thread's own copy, as
// `bundle --deal` builds it, in x-deal-sale/v0; and the openings of the
// registrations in the x-deal-v0 section's sale_threads. A sale with a thread
// opened before threads were registered says so
// (threads_predate_registration): its list of threads is not whole.
func (s *dealSession) dealSaleBundle(ctx context.Context, saleID, verify string) (map[string]interface{}, map[string]interface{}, error) {
	if err := s.useDeal(ctx, saleID, false); err != nil {
		return nil, nil, err
	}
	events, err := s.load(ctx, saleID)
	if err != nil {
		return nil, nil, err
	}
	predate, err := s.unregisteredThreads(ctx, saleID)
	if err != nil {
		return nil, nil, err
	}
	var entries []interface{}
	threads := map[string]interface{}{}
	counts := map[string]int{}
	var opened []openedThread
	for _, se := range events {
		th := se.Event.Thread
		if th == nil {
			continue
		}
		entry := map[string]interface{}{"registration": digestRef(se.Digest), "nonce": se.Event.Nonces["thread_ref"], "thread_id": th.ThreadID, "member": cli.SaleThreadNeverOpened}
		_, isOpen, err := s.threadSteps(ctx, th.ThreadID)
		if err != nil {
			return nil, nil, err
		}
		if isOpen {
			if err := s.t.Close(); err != nil {
				return nil, nil, err
			}
			s.t = nil
			if err := s.useDeal(ctx, th.ThreadID, false); err != nil {
				return nil, nil, err
			}
			tevents, err := s.load(ctx, th.ThreadID)
			if err != nil {
				return nil, nil, err
			}
			report, err := s.dealReportFor(ctx, tevents)
			if err != nil {
				return nil, nil, err
			}
			tb, err := s.dealReportBundle(ctx, tevents, report, cli.DealAudienceKeep, verify, true)
			if err != nil {
				return nil, nil, err
			}
			threads[th.ThreadID] = tb
			entry["member"] = cli.SaleThreadPresent
			authority, err := s.threadAuthority(ctx, th.ThreadID)
			if err != nil {
				return nil, nil, err
			}
			// The head is this copy's last record on the thread's log: the
			// report just sealed in it, read from the bundle as a verifier
			// reads it.
			raw, err := json.Marshal(tb)
			if err != nil {
				return nil, nil, err
			}
			decoded, err := cli.DecodeBundleJSON(raw)
			if err != nil {
				return nil, nil, err
			}
			head := cli.LastRecordDigest(decoded)
			opened = append(opened, openedThread{entry: entry, registration: se.Digest, authority: authority, head: head})
		}
		counts[entry["member"].(string)]++
		entries = append(entries, entry)
	}
	// Every opened thread's opening is on the sale's log (a crash may have
	// come between a thread's task authority and it); then the cut, which
	// grows the sale's log, so the checkpoint below is cut now and its
	// interval covers every thread's acts up to this bundle.
	if s.t != nil {
		if err := s.t.Close(); err != nil {
			return nil, nil, err
		}
		s.t = nil
	}
	if dealSealsSaleEvidence {
		if events, err = s.sealSaleEvidence(ctx, saleID, opened); err != nil {
			return nil, nil, err
		}
	} else if err := s.useDeal(ctx, saleID, false); err != nil {
		return nil, nil, err
	}
	b, cadence, err := s.dealAssemble(ctx, events, nil)
	if err != nil {
		return nil, nil, err
	}
	section := map[string]interface{}{"sale_threads": entries}
	if entries == nil {
		section["sale_threads"] = []interface{}{}
	}
	if predate {
		section["threads_predate_registration"] = true
	}
	b["extensions"] = map[string]interface{}{cli.DealCadenceExtension: cadence, cli.DealProfile: section, cli.DealSaleExtension: map[string]interface{}{"threads": threads}}
	if err = cli.VerifyProducedBundle(b, true); err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, nil, err
	}
	value, err := cli.DecodeBundleJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	verdict, _ := cli.BundleVerdict(value, nil)
	summary := map[string]interface{}{"verdict": verdict, "threads": len(entries), "present": counts[cli.SaleThreadPresent], "never_opened": counts[cli.SaleThreadNeverOpened]}
	if predate {
		summary["threads_predate_registration"] = true
	}
	return b, summary, nil
}

// unregisteredThreads reports whether this device holds a thread of the
// sale opened before threads were registered (its task authority names no
// registration).
func (s *dealSession) unregisteredThreads(ctx context.Context, saleID string) (bool, error) {
	authorities, err := s.index.steps(ctx, stepFilter{Kinds: []string{"task_authority"}})
	if err != nil {
		return false, err
	}
	var unregistered []string
	for _, st := range authorities {
		var ev dealEvent
		if json.Unmarshal([]byte(st.Local), &ev) == nil && ev.SaleAuthority != "" && ev.SaleRegistration == "" {
			unregistered = append(unregistered, ev.DealID)
		}
	}
	for _, dealID := range unregistered {
		open, err := s.index.steps(ctx, stepFilter{DealID: dealID, N: 1})
		if err != nil {
			return false, err
		}
		if len(open) == 0 {
			return false, sql.ErrNoRows
		}
		var first dealEvent
		if json.Unmarshal([]byte(open[0].Local), &first) == nil && first.Open != nil && first.Open.Sale == saleID {
			return true, nil
		}
	}
	return false, nil
}

// threadAuthority is a thread's opening task-authority record digest, from
// this device's store.
func (s *dealSession) threadAuthority(ctx context.Context, threadID string) (string, error) {
	steps, err := s.index.steps(ctx, stepFilter{DealID: threadID, Kinds: []string{"task_authority"}})
	if err != nil {
		return "", err
	}
	if len(steps) == 0 {
		return "", sql.ErrNoRows
	}
	return steps[0].Digest, nil
}

// openedThread is an opened thread a sale bundle carries: its sale_threads
// entry, its registration's record digest, its task-authority record digest
// and its last record's digest, read after the bundle step sealed its report.
type openedThread struct {
	entry                         map[string]interface{}
	registration, authority, head string
}

// sealSaleEvidence seals, on the sale's log, each opened thread's opening
// (if a crash came between its task authority and it) and then the cut,
// naming each thread's last record, which already includes the report the
// bundle step sealed in it. The cut grows the sale's log, so the checkpoint
// cut after it is new: the order is the threads' reports, the cut, the
// checkpoint. It adds each entry's openings and returns the sale's events,
// leaving the session on the sale's log.
func (s *dealSession) sealSaleEvidence(ctx context.Context, saleID string, opened []openedThread) ([]sealedEvent, error) {
	cut := &dealSaleCut{Heads: []dealThreadHead{}}
	for _, o := range opened {
		if err := s.sealThreadOpened(ctx, saleID, o.registration, o.authority); err != nil {
			return nil, err
		}
		cut.Heads = append(cut.Heads, dealThreadHead{Registration: o.registration, Head: o.head})
	}
	if s.t == nil {
		if err := s.useDeal(ctx, saleID, false); err != nil {
			return nil, err
		}
	}
	events, err := s.load(ctx, saleID)
	if err != nil {
		return nil, err
	}
	cut.Heads = dealSaleCutHeads(cut.Heads)
	cutEvent, err := s.seal(ctx, saleID, events, dealEvent{Kind: "sale_cut", SaleCut: cut})
	if err != nil {
		return nil, err
	}
	events = append(events, cutEvent)
	for i, o := range opened {
		for _, se := range events {
			if t := se.Event.ThreadOpened; t != nil && t.Registration == o.registration {
				o.entry["opened"] = map[string]interface{}{"nonce": se.Event.Nonces["task_authority"]}
			}
		}
		o.entry["head"] = map[string]interface{}{"nonce": cutEvent.Event.Nonces[fmt.Sprintf("head_%d", i)], "record_digest": o.head}
	}
	return events, nil
}
