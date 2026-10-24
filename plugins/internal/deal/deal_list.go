package deal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/spf13/cobra"
)

// dealListing is one deal in the profile's store, as `deal list` prints it:
// identifiers, timestamps and, with --json, a plain summary. A sale (`deal
// sale new`) is listed with kind "sale": its log is in the same store,
// though it is not a deal.
type dealListing struct {
	DealID       string `json:"deal_id"`
	Kind         string `json:"kind"`  // "deal" or "sale"
	State        string `json:"state"` // "open", or "closed" once a final close is indexed
	OpenedAt     string `json:"opened_at"`
	LastRecordAt string `json:"last_record_at"`
	// CounterpartyFingerprint is the payee fingerprint of the deal's latest
	// counterparty_profile step (profileKeyFor), or null when it has none.
	CounterpartyFingerprint *string `json:"counterparty_fingerprint"`
	LogID                   string  `json:"log_id"`
	// Result is a deal's result (dealResult): bought, sold, called_off,
	// not_chosen or open. A sale has none: its threads carry theirs.
	Result string `json:"result,omitempty"`
	// Summary is the deal in plain words, read from the steps the list
	// reads. It is the user's own view: what they asked, the counterparty's
	// name and the amount are private, and no shared copy carries them.
	Summary dealSummary `json:"summary"`
	// Say is the deal's line for "show my receipts", from the wording pack
	// (listingSay); only `deal list --json` fills it.
	Say string `json:"say,omitempty"`
	// events are the steps the listing was read from.
	events []sealedEvent
}

// dealSummary is a deal in plain words: what the user asked (the words
// `deal report` gives as asked), with whom, the amount and the outcome, and
// those four as one line. It is computed from the sealed steps alone: it
// seals nothing and cuts no checkpoint, unlike `deal report`.
type dealSummary struct {
	What string `json:"what"`
	// Counterparty is the name the opening step sealed as who.name, or
	// dealNotRecorded: a name is never read out of free text.
	Counterparty string `json:"counterparty"`
	// Amount is nil when no step records one.
	Amount  *dealSummaryAmount `json:"amount"`
	Outcome string             `json:"outcome"`
	Text    string             `json:"text"`
}

// dealSummaryAmount is an amount as recorded, in minor units. Basis says
// which: "paid" is the net of the sealed acts in the first act's currency
// (`deal report`'s money net_minor; acts in another currency are counted in
// OtherCurrencyActs, as the report counts them, never summed), and
// "opening_terms" the opening step's price when no act moved money (a later
// change of terms is not read).
type dealSummaryAmount struct {
	AmountMinor       int64  `json:"amount_minor"`
	Currency          string `json:"currency"`
	Basis             string `json:"basis"`
	OtherCurrencyActs int    `json:"other_currency_acts,omitempty"`
}

const dealNotRecorded = "not recorded"

func dealListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "One line per deal in the profile's store: deal id, kind, state, opened and last-record times, counterparty fingerprint, log id; --json adds a plain summary (what, with whom, amount, outcome); --since/--until pick the deals active in a range, and with --json add its totals and who was told what; read-only", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		since, _ := c.Flags().GetString("since")
		until, _ := c.Flags().GetString("until")
		bounds, ranged, err := listBounds(since, until)
		if err != nil {
			return err
		}
		p, err := cli.Selected(c)
		if err != nil {
			return err
		}
		s, err := openDealStoreRead(c.Context(), p)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		logs, err := s.dealLogs(c.Context())
		if err != nil {
			return err
		}
		asJSON, _ := c.Flags().GetBool("json")
		var list []dealListing
		if ranged {
			r, err := dealListInRange(logs, bounds)
			if err != nil {
				return err
			}
			if asJSON {
				key, vals := "say.list.other", cli.V{"count": fmt.Sprint(r.Totals.Deals)}
				if r.Totals.Deals == 1 {
					key = "say.list.one"
				}
				r.Narration = cli.NarrateWith(c, func(w cli.Wording) cli.Narration {
					sayListings(w, r.Deals)
					return cli.Narration{Voice: cli.VoiceReport, Say: w.Line(key, vals)}
				})
				return json.NewEncoder(c.OutOrStdout()).Encode(r)
			}
			list = r.Deals
		} else {
			list = make([]dealListing, 0, len(logs))
			for _, l := range logs {
				list = append(list, dealListingOf(l.id, l.events))
			}
			if asJSON {
				// A plain array carries no narration of its own: each
				// listing has its line, with the words the profile pins.
				w, _ := cli.ActiveWording(p)
				sayListings(w, list)
				return json.NewEncoder(c.OutOrStdout()).Encode(list)
			}
		}
		for _, l := range list {
			fp := "-"
			if l.CounterpartyFingerprint != nil {
				fp = *l.CounterpartyFingerprint
			}
			if _, err = fmt.Fprintln(c.OutOrStdout(), strings.Join([]string{l.DealID, l.Kind, l.State, l.OpenedAt, l.LastRecordAt, fp, l.LogID}, "\t")); err != nil {
				return err
			}
		}
		return nil
	}}
	cmd.Flags().Bool("json", false, "Emit the deals as one JSON array, each with a plain summary, instead of tab-separated lines; with --since or --until, one object: the range, its deals, totals and disclosures")
	cmd.Flags().String("since", "", "List only deals with a step at or after this date (2026-10-01, UTC) or time (2026-10-01T00:00:00-07:00)")
	cmd.Flags().String("until", "", "List only deals with a step before this date or time (exclusive), each as it stood then")
	return cmd
}

// sayListings fills each listing's line.
func sayListings(w cli.Wording, list []dealListing) {
	for i := range list {
		list[i].Say = listingSay(w, list[i], list[i].events)
	}
}

// dealSummaryOf reads the summary from a deal's steps. Its outcome is the
// last close step's, as `deal report` reads it, or "open".
func dealSummaryOf(events []sealedEvent) dealSummary {
	sum := dealSummary{What: dealNotRecorded, Counterparty: dealNotRecorded, Outcome: "open"}
	currency := ""
	if open := events[0].Event.Open; open != nil {
		if strings.TrimSpace(open.Intent.Verbatim) != "" {
			sum.What = open.Intent.Verbatim
		}
		// The errand's counterparty is the last leg's.
		if leg := legWho(events); strings.TrimSpace(leg.Name) != "" {
			sum.Counterparty = leg.Name
		}
		currency = open.Terms.Currency
		if open.Terms.PriceMinor != nil {
			sum.Amount = &dealSummaryAmount{AmountMinor: *open.Terms.PriceMinor, Currency: strings.ToUpper(currency), Basis: "opening_terms"}
		}
	}
	if m := buildDealMoney(events, currency); m != nil {
		sum.Amount = &dealSummaryAmount{AmountMinor: m.NetMinor, Currency: m.Currency, Basis: "paid", OtherCurrencyActs: m.Other}
	}
	for _, se := range events {
		if se.Event.Kind == "close" && se.Event.Close != nil {
			sum.Outcome = se.Event.Close.Outcome
		}
	}
	amount := "amount not recorded"
	if a := sum.Amount; a != nil {
		amount = formatMoney(a.AmountMinor, a.Currency) + " in the opening terms"
		if a.Basis == "paid" {
			amount = "net " + formatMoney(a.AmountMinor, a.Currency) + " paid"
		}
		if a.OtherCurrencyActs > 0 {
			amount += fmt.Sprintf(", and %d act(s) in another currency", a.OtherCurrencyActs)
		}
	}
	with := "with " + sum.Counterparty
	if sum.Counterparty == dealNotRecorded {
		with = "counterparty not recorded"
	}
	sum.Text = strings.Join([]string{sum.What, with, amount, sum.Outcome}, " · ")
	return sum
}

// dealListings lists every deal in the store (dealLogs), in deal id order.
func (s *dealSession) dealListings(ctx context.Context) ([]dealListing, error) {
	logs, err := s.dealLogs(ctx)
	if err != nil {
		return nil, err
	}
	list := make([]dealListing, 0, len(logs))
	for _, l := range logs {
		list = append(list, dealListingOf(l.id, l.events))
	}
	return list, nil
}

// dealLog is one deal's steps, as dealLogs reads them.
type dealLog struct {
	id     string
	events []sealedEvent
}

// dealLogs reads every deal's steps from the store's index, in deal id
// order, and checks each row the way `load` does (one strict record whose
// deal, step number and kind are the row's, chained to the step before by
// its digest), without re-verifying the steps against the deal's log
// (`deal export` does). A last step indexed but not yet in the log (a crash
// between the two writes, which the next write command recovers) is read.
// A store with no deal yet holds none.
func (s *dealSession) dealLogs(ctx context.Context) (_ []dealLog, err error) {
	logs := []dealLog{}
	steps, err := s.index.steps(ctx, stepFilter{})
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return logs, nil
		}
		return nil, err
	}
	var id, prev string
	var events []sealedEvent
	flush := func() {
		if len(events) > 0 {
			logs = append(logs, dealLog{id: id, events: events})
		}
	}
	for _, st := range steps {
		rowID, n, kind, digest, local := st.DealID, st.N, st.Kind, st.Digest, st.Local
		if rowID != id {
			flush()
			id, prev, events = rowID, "", nil
		}
		var ev dealEvent
		if err = cli.DecodeJSON([]byte(local), &ev); err != nil {
			return nil, cli.ErrConflict
		}
		if ev.DealID != id || ev.N != n || n != int64(len(events)+1) || ev.Kind != kind || ev.Prev != prev {
			return nil, cli.ErrConflict
		}
		events = append(events, sealedEvent{Event: ev})
		prev = digest
	}
	flush()
	return logs, nil
}

func dealListingOf(dealID string, events []sealedEvent) dealListing {
	l := dealListing{
		DealID: dealID, Kind: "deal", State: "open",
		OpenedAt: events[0].Event.At, LastRecordAt: events[len(events)-1].Event.At,
		LogID: cli.DealLogID(dealID), Summary: dealSummaryOf(events), events: events,
	}
	if cli.SaleIDPattern.MatchString(dealID) {
		l.Kind = "sale"
	} else {
		l.Result = dealResult(events)
	}
	if dealFinallyClosed(events) {
		l.State = "closed"
	}
	for _, se := range events {
		if cp := se.Event.CounterpartyProfile; se.Event.Kind == "counterparty_profile" && cp != nil {
			l.CounterpartyFingerprint = &cp.Payee
		}
	}
	return l
}
