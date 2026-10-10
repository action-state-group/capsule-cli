package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// A sale is one item a user sells to one of several buyers. It is its own
// log: a root (what is for sale, the seller's request, and the item
// reference by commitment) and the sale's one task authority. Each buyer's
// negotiation is its own deal, a thread opened under the sale with
// `deal open --sale`: the thread is sealed under the sale's task authority
// (its task authority commits to that one's digest under its own nonce,
// sale_authority_commitment), and each of its checks seals the item
// reference as a commitment salted per check, so no buyer's copy carries the
// reference or anything equal across threads.
// The plain reference goes only to the profile's own rules checker
// (item_ref), which is how it holds a sale to one accepted commitment.

var saleIDPattern = regexp.MustCompile(`^sale-[0-9a-f]{16}$`)

func dealSaleCommands() *cobra.Command {
	sale := &cobra.Command{Use: "sale", Short: "A sale to one of several buyers: one item, one task authority, a thread per buyer"}
	sale.AddCommand(dealSaleNewCommand(), dealSaleExportCommand(), dealSaleBundleCommand())
	return sale
}

func dealSaleNewCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "new", Short: "Seal a sale: what is for sale, the seller's request and its one task authority", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var o dealOpen
		if err = decodeJSONAs("--input", raw, &o); err != nil {
			return err
		}
		if o.Who != (dealWho{}) || len(o.Claims) > 0 {
			return inputError("a sale names no buyer and holds no claims: each buyer's thread does (deal open --sale)")
		}
		if o.Intent.PartyRole == "" {
			o.Intent.PartyRole = dealRoleSeller
		}
		if o.Intent.PartyRole != dealRoleSeller {
			return inputError("a sale is made by the seller: intent.party_role is seller")
		}
		// Fixed by capsulectl, never taken from the input file.
		o.Skill, o.Materiality, o.Sale, o.SaleAuthority = nil, dealMateriality{}, "", ""
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
			return inputError("a sale names its item and currency (terms.item, terms.currency)")
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
			return output(c, map[string]any{"sale_id": saleID, "task_authority_digest": authority.Digest, "checkpoint": cp,
				"next": "open each buyer's thread with deal open --sale " + saleID + " (its input names the buyer and the terms with them)"})
		})
	}}
	cmd.Flags().String("input", "", "Sale JSON: type, intent (the seller's request and limits), terms (item, asking price, currency), recourse")
	return cmd
}

func dealSaleExportCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "export", Short: "Write the sale's sealed records as one JSON array, in order", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		saleID, _ := c.Flags().GetString("sale")
		path, _ := c.Flags().GetString("output")
		if !saleIDPattern.MatchString(saleID) || path == "" {
			return inputError("--sale (a sale id as printed by `deal sale new`) and --output are required")
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
			if err = atomicFile(path, b, false); err != nil {
				return err
			}
			return output(c, map[string]any{"sale_id": saleID, "records": len(records), "output": path})
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
	if !saleIDPattern.MatchString(saleID) {
		return dealOpen{}, "", inputError("--sale must be a sale id as printed by `deal sale new`: sale- followed by 16 hex characters")
	}
	var local, digest string
	err := s.db.QueryRowContext(ctx, `SELECT local FROM deal_steps WHERE deal_id=? AND n=1 AND kind='sale'`, saleID).Scan(&local)
	if errors.Is(err, sql.ErrNoRows) {
		return dealOpen{}, "", inputError("unknown sale: " + saleID)
	}
	if err != nil {
		return dealOpen{}, "", err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT record_digest FROM deal_steps WHERE deal_id=? AND n=2 AND kind='task_authority'`, saleID).Scan(&digest); err != nil {
		return dealOpen{}, "", errors.Join(inputError("the sale "+saleID+" has no task authority"), err)
	}
	var ev dealEvent
	if err = json.Unmarshal([]byte(local), &ev); err != nil || ev.Open == nil || ev.Open.ItemRef == "" {
		return dealOpen{}, "", errors.Join(inputError("the sale "+saleID+" cannot be read"), err)
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
		return inputError("a sale's thread is under the sale's request and limits: leave intent out of its input")
	}
	if o.Type == "" {
		o.Type = sale.Type
	}
	switch {
	case o.Type != sale.Type:
		return inputError("a sale's thread has the sale's deal type, " + sale.Type)
	case o.Terms.Item != "" && !strings.EqualFold(strings.TrimSpace(o.Terms.Item), strings.TrimSpace(sale.Terms.Item)):
		return inputError("a sale's thread is about the sale's item, " + sale.Terms.Item)
	case o.Terms.Currency != "" && !strings.EqualFold(o.Terms.Currency, sale.Terms.Currency):
		return inputError("a sale's thread is in the sale's currency, " + sale.Terms.Currency)
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
		return inputError("the sale's task authority is not the one this thread was opened under")
	}
	return nil
}

// dealThreadRegistrationVersion marks a step sealed after a sale's threads
// were registered (dealEvent.ThreadRegistration).
const dealThreadRegistrationVersion = "1"

// dealRegistersThreads is whether `deal open --sale` registers the thread
// on the sale's log; tests turn it off to seal a thread as an earlier
// release did.
var dealRegistersThreads = true

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
	for _, se := range events {
		th := se.Event.Thread
		if th == nil || (threadID != "" && th.ThreadID != threadID) {
			continue
		}
		var held int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_keys WHERE deal_id=?`, th.ThreadID).Scan(&held); err != nil {
			return "", "", err
		}
		if held == 0 {
			return th.ThreadID, se.Digest, nil
		}
		if threadID != "" {
			return "", "", inputError("this sale already registered deal " + threadID)
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
