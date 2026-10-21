package plugin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/spf13/cobra"
)

func dealSaleCommands() *cobra.Command {
	sale := &cobra.Command{Use: "sale", Short: "A sale to one of several buyers: one item, one task authority, a thread per buyer"}
	sale.AddCommand(dealSaleNewCommand(), dealSaleExportCommand(), dealSaleBundleCommand(), dealSaleCloseCommand())
	return sale
}

func dealSaleNewCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "new", Short: "Seal a sale: what is for sale, the seller's request and its one task authority", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := cli.ReadInput(path)
		if err != nil {
			return err
		}
		var o dealOpen
		if err = cli.DecodeJSONAs("--input", raw, &o); err != nil {
			return err
		}
		if o.Who != (dealWho{}) || len(o.Claims) > 0 {
			return cli.InputError("a sale names no buyer and holds no claims: each buyer's thread does (deal open --sale)")
		}
		if o.Intent.PartyRole == "" {
			o.Intent.PartyRole = dealRoleSeller
		}
		if o.Intent.PartyRole != dealRoleSeller {
			return cli.InputError("a sale is made by the seller: intent.party_role is seller")
		}
		// Fixed by capsulectl, never taken from the input file.
		o.Skill, o.Materiality, o.Sale, o.SaleAuthority = nil, cli.DealMateriality{}, "", ""
		o.Records = recordsTyped
		o.Who = dealWho{Name: "sale"} // validate wants a counterparty; the sale record carries none
		if err = o.validate(); err != nil {
			return err
		}
		o.Who = dealWho{}
		if err = normalizeOpen(&o); err != nil {
			return err
		}
		if strings.TrimSpace(o.Terms.Item) == "" || o.Terms.Currency == "" {
			return cli.InputError("a sale names its item and currency (terms.item, terms.currency)")
		}
		item := make([]byte, 32)
		if _, err = rand.Read(item); err != nil {
			return err
		}
		o.ItemRef = hex.EncodeToString(item)
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			id := make([]byte, 8)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			saleID := "sale-" + hex.EncodeToString(id)
			if err := s.useDeal(ctx, saleID, true); err != nil {
				return err
			}
			root, err := s.seal(ctx, saleID, nil, dealEvent{Kind: "sale", Open: &o})
			if err != nil {
				return err
			}
			intent := o.Intent
			authority, err := s.seal(ctx, saleID, []sealedEvent{root}, dealEvent{Kind: "task_authority", TaskAuthority: &intent})
			if err != nil {
				return err
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			out := map[string]any{"sale_id": saleID, "task_authority_digest": authority.Digest, "checkpoint": cp,
				"next": "open each buyer's thread with deal open --sale " + saleID + " (its input names the buyer and the terms with them)"}
			return cli.Output(c, cli.Speak(out, s.p, func(w cli.Wording) cli.Narration {
				if o.Terms.PriceMinor == nil {
					return line(w.Line("say.sale.new.no_price", cli.V{"item": shortTitle(o.Terms.Item)}))
				}
				return line(w.Line("say.sale.new", cli.V{"item": shortTitle(o.Terms.Item), "price": formatMoney(*o.Terms.PriceMinor, o.Terms.Currency)}))
			}))
		})
	}}
	cmd.Flags().String("input", "", "Sale JSON: type, intent (the seller's request and limits), terms (item, asking price, currency), recourse")
	return cmd
}

func dealSaleCloseCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "close", Short: "End a sale: no buyer's thread opens on it after this; a thread already open goes on", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		saleID, _ := c.Flags().GetString("sale")
		if !cli.SaleIDPattern.MatchString(saleID) {
			return cli.InputError("--sale (a sale id as printed by `deal sale new`) is required")
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			se, cp, open, err := s.closeSale(ctx, saleID)
			if err != nil {
				return err
			}
			return cli.Output(c, map[string]any{"sale_id": saleID, "capsule_id": se.CapsuleID, "checkpoint": cp,
				"status": saleClosedStatus(open), "open_negotiations": open,
				"next": "deal sale bundle --sale " + saleID + " still writes the sale's copy"})
		})
	}}
	cmd.Flags().String("sale", "", "Sale ID from `deal sale new`")
	return cmd
}

// saleClosedStatus is the line `deal sale close` prints: the sale takes no
// new buyer, and how many buyers' threads are still negotiating.
func saleClosedStatus(open int) string {
	switch open {
	case 0:
		return "stops taking new buyers; no negotiations still open"
	case 1:
		return "stops taking new buyers; 1 negotiation still open"
	}
	return fmt.Sprintf("stops taking new buyers; %d negotiations still open", open)
}

// closeSale seals a sale's sale_close and cuts its checkpoint
// (capsule-cli#271), so the cadence takes the sale's log out of its live
// set once a witnessed tick covers it. Every opened thread's opening is
// sealed first (a crash may have come between a thread's task authority
// and it), so a sale_cut is the only record that ever follows the close.
// It leaves the session on the sale's log, and returns how many of the
// sale's buyers' threads are still open: opened, with no final close.
func (s *dealSession) closeSale(ctx context.Context, saleID string) (sealedEvent, interface{}, int, error) {
	if _, _, err := s.saleOf(ctx, saleID); err != nil {
		return sealedEvent{}, nil, 0, err
	}
	if err := s.useDeal(ctx, saleID, false); err != nil {
		return sealedEvent{}, nil, 0, err
	}
	events, err := s.load(ctx, saleID)
	if err != nil {
		return sealedEvent{}, nil, 0, err
	}
	if saleClosed(events) {
		return sealedEvent{}, nil, 0, cli.InputError("the sale " + saleID + " is already closed")
	}
	type registration struct{ digest, thread string }
	var registrations []registration
	for _, se := range events {
		if th := se.Event.Thread; th != nil {
			registrations = append(registrations, registration{se.Digest, th.ThreadID})
		}
	}
	open := 0
	for _, r := range registrations {
		opened, still, err := s.threadState(ctx, r.thread)
		if err != nil {
			return sealedEvent{}, nil, 0, err
		}
		if still {
			open++
		}
		if !opened {
			continue
		}
		authority, err := s.threadAuthority(ctx, r.thread)
		if err != nil {
			return sealedEvent{}, nil, 0, err
		}
		if err := s.sealThreadOpened(ctx, saleID, r.digest, authority); err != nil {
			return sealedEvent{}, nil, 0, err
		}
	}
	if events, err = s.load(ctx, saleID); err != nil {
		return sealedEvent{}, nil, 0, err
	}
	se, err := s.seal(ctx, saleID, events, dealEvent{Kind: "sale_close"})
	if err != nil {
		return sealedEvent{}, nil, 0, err
	}
	cp, err := s.milestone(ctx)
	if err != nil {
		return sealedEvent{}, nil, 0, err
	}
	return se, cp, open, nil
}

// threadState is whether a buyer's thread has opened (its task authority
// is sealed), and whether it is still open: opened, with no final close.
// Both are read from its sealed steps; "final close" is finalClose's rule
// (a close whose outcome is not open), applied to the index's steps.
func (s *dealSession) threadState(ctx context.Context, threadID string) (opened, still bool, err error) {
	steps, err := s.index.steps(ctx, stepFilter{DealID: threadID})
	if err != nil {
		return false, false, err
	}
	closed := false
	for _, st := range steps {
		switch st.Kind {
		case "task_authority":
			opened = true
		case "close":
			var ev dealEvent
			if err := json.Unmarshal([]byte(st.Local), &ev); err != nil {
				return false, false, cli.ErrConflict
			}
			closed = closed || (ev.Close != nil && ev.Close.Outcome != "open")
		}
	}
	return opened, opened && !closed, nil
}

func dealSaleExportCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "export", Short: "Write the sale's sealed records as one JSON array, in order", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		saleID, _ := c.Flags().GetString("sale")
		path, _ := c.Flags().GetString("output")
		if !cli.SaleIDPattern.MatchString(saleID) || path == "" {
			return cli.InputError("--sale (a sale id as printed by `deal sale new`) and --output are required")
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			if err := s.useDeal(ctx, saleID, false); err != nil {
				return err
			}
			events, err := s.load(ctx, saleID)
			if err != nil {
				return err
			}
			records := make([]json.RawMessage, 0, len(events))
			for i, se := range events {
				payload, _, err := encodeDealRecord(se.Event, events[:i], s.dkey)
				if err != nil {
					return err
				}
				records = append(records, payload)
			}
			b, err := json.Marshal(records)
			if err != nil {
				return err
			}
			if err = cli.AtomicFile(path, b, false); err != nil {
				return err
			}
			return cli.Output(c, map[string]any{"sale_id": saleID, "records": len(records), "output": path})
		})
	}}
	cmd.Flags().String("sale", "", "Sale ID from `deal sale new`")
	cmd.Flags().String("output", "", "New file for the JSON array of records")
	return cmd
}

// saleOf is a sale's root (what is for sale, the request, the item
// reference) and the digest of its one task authority, read from this
// device's store.
func (s *dealSession) saleOf(ctx context.Context, saleID string) (dealOpen, string, error) {
	if !cli.SaleIDPattern.MatchString(saleID) {
		return dealOpen{}, "", cli.InputError("--sale must be a sale id as printed by `deal sale new`: sale- followed by 16 hex characters")
	}
	root, err := s.index.steps(ctx, stepFilter{DealID: saleID, N: 1, Kinds: []string{"sale"}})
	if err != nil {
		return dealOpen{}, "", err
	}
	if len(root) == 0 {
		return dealOpen{}, "", cli.InputError("unknown sale: " + saleID)
	}
	authority, err := s.index.steps(ctx, stepFilter{DealID: saleID, N: 2, Kinds: []string{"task_authority"}})
	if err == nil && len(authority) == 0 {
		err = sql.ErrNoRows
	}
	if err != nil {
		return dealOpen{}, "", errors.Join(cli.InputError("the sale "+saleID+" has no task authority"), err)
	}
	digest := authority[0].Digest
	var ev dealEvent
	if err = json.Unmarshal([]byte(root[0].Local), &ev); err != nil || ev.Open == nil || ev.Open.ItemRef == "" {
		return dealOpen{}, "", errors.Join(cli.InputError("the sale "+saleID+" cannot be read"), err)
	}
	return *ev.Open, digest, nil
}

// dealThreadCounterpartyVersion marks a step whose check on a sale's thread
// seals the thread's buyer as its counterparty when the check names no one
// (rules keyed on the counterparty, such as one commitment per sale, need a
// target). A step sealed before has none and re-derives unchanged.
const dealThreadCounterpartyVersion = "1"

// underSale puts a buyer's thread under its sale: the sale's request and
// task authority, its deal type, item and currency, and the typed records a
// seller's deal is sealed in. An input that states another request, type,
// item or currency is refused rather than overridden.
func underSale(o *dealOpen, sale dealOpen, saleID, authority string) error {
	if o.Intent.Verbatim != "" && !reflect.DeepEqual(o.Intent, sale.Intent) {
		return cli.InputError("a sale's thread is under the sale's request and limits: leave intent out of its input")
	}
	if o.Type == "" {
		o.Type = sale.Type
	}
	switch {
	case o.Type != sale.Type:
		return cli.InputError("a sale's thread has the sale's deal type, " + sale.Type)
	case o.Terms.Item != "" && !strings.EqualFold(strings.TrimSpace(o.Terms.Item), strings.TrimSpace(sale.Terms.Item)):
		return cli.InputError("a sale's thread is about the sale's item, " + sale.Terms.Item)
	case o.Terms.Currency != "" && !strings.EqualFold(o.Terms.Currency, sale.Terms.Currency):
		return cli.InputError("a sale's thread is in the sale's currency, " + sale.Terms.Currency)
	}
	if o.Terms.Item == "" {
		o.Terms.Item = sale.Terms.Item
	}
	o.Terms.Currency = sale.Terms.Currency
	if o.Terms.Quantity == 0 {
		o.Terms.Quantity = sale.Terms.Quantity
	}
	if o.Recourse == (dealRecourse{}) {
		o.Recourse = sale.Recourse
	}
	o.Intent = sale.Intent
	o.Records = recordsTyped
	o.ItemRef, o.Sale, o.SaleAuthority = sale.ItemRef, saleID, authority
	return nil
}

// saleAuthorityOpening is, on a sale's thread, the opening of its
// sale_authority_commitment: the record that carries it, its nonce, and the
// sale's task-authority digest. Only the user's own copy and an
// adjudicator's carry it: with it, a reader can tie the thread to the sale.
func saleAuthorityOpening(events []sealedEvent) map[string]interface{} {
	for _, se := range events {
		if se.Event.Kind == "task_authority" && se.Event.SaleAuthority != "" {
			return map[string]interface{}{"record_digest": se.Digest, "nonce": se.Event.Nonces["sale_authority"], "text": se.Event.SaleAuthority}
		}
	}
	return nil
}

// saleUnchanged refuses a step on a sale's thread when the sale's task
// authority is no longer the one the thread was opened under.
func (s *dealSession) saleUnchanged(ctx context.Context, events []sealedEvent) error {
	open := events[0].Event.Open
	if open.Sale == "" {
		return nil
	}
	_, authority, err := s.saleOf(ctx, open.Sale)
	if err != nil {
		return err
	}
	if authority != open.SaleAuthority {
		return cli.InputError("the sale's task authority is not the one this thread was opened under")
	}
	return nil
}

// dealThreadRegistrationVersion marks a step sealed after a sale's threads
// were registered (dealEvent.ThreadRegistration).
const dealThreadRegistrationVersion = "1"

// dealRegistersThreads is whether `deal open --sale` registers the thread
// on the sale's log; tests turn it off to seal a thread as an earlier
// release did. Off, registerThread is skipped, and with it the refusal to
// open a thread on a closed sale.
var dealRegistersThreads = true

// dealSealsSaleEvidence is whether a sale's log records each thread's
// opening and a sale bundle's cut; tests turn it off to stand in for a copy
// made before they were.
var dealSealsSaleEvidence = true

// dealSaleCutHeads is a test hook: the heads a sale bundle's cut seals,
// from the ones it would.
var dealSaleCutHeads = func(heads []dealThreadHead) []dealThreadHead { return heads }

// dealThread is a sale's registration of one thread: the thread's deal id,
// sealed as a commitment on the sale's own log.
type dealThread struct {
	ThreadID string `json:"thread_id"`
}

// registerThread registers a thread on the sale's log before the thread
// opens, so no thread of a sale exists unregistered, and cuts the sale's
// checkpoint. It returns the thread's id and the registration's record
// digest. With no id given, a registration whose thread never opened (an
// open that failed after registering) is reused, not sealed again. It
// leaves the session on the sale's log.
func (s *dealSession) registerThread(ctx context.Context, saleID, threadID string) (string, string, error) {
	if err := s.useDeal(ctx, saleID, false); err != nil {
		return "", "", err
	}
	events, err := s.load(ctx, saleID)
	if err != nil {
		return "", "", err
	}
	// Here, under the store's lock and before a registration is reused,
	// not in saleOf, which every step of an open thread reads: a thread
	// already open goes on after its sale closes.
	if saleClosed(events) {
		return "", "", cli.InputError("the sale " + saleID + " is closed: no buyer's thread opens on it")
	}
	for _, se := range events {
		th := se.Event.Thread
		if th == nil || (threadID != "" && th.ThreadID != threadID) {
			continue
		}
		// Reused only when nothing of its thread was sealed: an open that
		// stopped before its baseline (its key row may already be written).
		// A thread with a baseline and no task authority is not reused; its
		// registration stays never opened.
		steps, _, err := s.threadSteps(ctx, th.ThreadID)
		if err != nil {
			return "", "", err
		}
		if steps == 0 {
			return th.ThreadID, se.Digest, nil
		}
		if threadID != "" {
			return "", "", cli.InputError("this sale already registered deal " + threadID)
		}
	}
	if threadID == "" {
		id := make([]byte, 8)
		if _, err := rand.Read(id); err != nil {
			return "", "", err
		}
		threadID = "deal-" + hex.EncodeToString(id)
	}
	se, err := s.seal(ctx, saleID, events, dealEvent{Kind: "thread", Thread: &dealThread{ThreadID: threadID}})
	if err != nil {
		return "", "", err
	}
	if _, err := s.milestone(ctx); err != nil {
		return "", "", err
	}
	return threadID, se.Digest, nil
}

// threadSteps is how many steps of a thread this device sealed, and whether
// one is its task authority: a thread is opened once its task authority is
// sealed, never by its key row alone (written before anything is sealed).
func (s *dealSession) threadSteps(ctx context.Context, threadID string) (int, bool, error) {
	steps, err := s.index.steps(ctx, stepFilter{DealID: threadID})
	if err != nil {
		return 0, false, err
	}
	authority := false
	for _, st := range steps {
		authority = authority || st.Kind == "task_authority"
	}
	return len(steps), authority, nil
}

// dealOpenStopsAt is a test hook: the open stops with its error at a stage
// ("keyed": the deal's key row is written; "baseline": the baseline is
// sealed), standing in for a crash there.
var dealOpenStopsAt = func(stage string) error { return nil }

// dealThreadOpened is a sale's evidence that a registered thread opened.
type dealThreadOpened struct {
	// Registration is the record digest of the thread's registration.
	Registration string `json:"registration"`
	// TaskAuthority is the thread's task-authority record digest.
	TaskAuthority string `json:"task_authority"`
}

// dealSaleCut is the cut a sale bundle is made at.
type dealSaleCut struct {
	Heads []dealThreadHead `json:"heads"`
}

// dealThreadHead is one opened thread at a cut: its registration's record
// digest, and its last record's digest then.
type dealThreadHead struct {
	Registration string `json:"registration"`
	Head         string `json:"head"`
}

// sealThreadOpened seals, on the sale's log, the evidence that the thread
// registered as registration opened with the task authority whose record
// digest is authority, unless the log already holds it, and cuts the sale's
// checkpoint. It leaves the session on the sale's log.
func (s *dealSession) sealThreadOpened(ctx context.Context, saleID, registration, authority string) error {
	if s.t != nil {
		if err := s.t.Close(); err != nil {
			return err
		}
		s.t = nil
	}
	if err := s.useDeal(ctx, saleID, false); err != nil {
		return err
	}
	events, err := s.load(ctx, saleID)
	if err != nil {
		return err
	}
	for _, se := range events {
		if o := se.Event.ThreadOpened; o != nil && o.Registration == registration {
			return nil
		}
	}
	if _, err = s.seal(ctx, saleID, events, dealEvent{Kind: "thread_opened", ThreadOpened: &dealThreadOpened{Registration: registration, TaskAuthority: authority}}); err != nil {
		return err
	}
	_, err = s.milestone(ctx)
	return err
}
