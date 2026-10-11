package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// dealListing is one deal in the profile's store, as `deal list` prints it:
// identifiers and timestamps only. A sale (`deal sale new`) is listed with
// kind "sale": its log is in the same store, though it is not a deal.
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
}

func dealListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "One line per deal in the profile's store: deal id, kind, state, opened and last-record times, counterparty fingerprint, log id; read-only", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, err := selected(c)
		if err != nil {
			return err
		}
		s, err := openDealStoreRead(p)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		list, err := s.dealListings(c.Context())
		if err != nil {
			return err
		}
		if asJSON, _ := c.Flags().GetBool("json"); asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(list)
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
	cmd.Flags().Bool("json", false, "Emit the deals as one JSON array instead of tab-separated lines")
	return cmd
}

// dealListings reads every deal's steps from the store's index, in deal id
// order, and checks each row the way `load` does (one strict record whose
// deal, step number and kind are the row's, chained to the step before by
// its digest), without re-verifying the steps against the deal's log
// (`deal export` does). A last step indexed but not yet in the log (a crash
// between the two writes, which the next write command recovers) is listed.
// A store with no deal yet lists none.
func (s *dealSession) dealListings(ctx context.Context) (_ []dealListing, err error) {
	list := []dealListing{}
	rows, err := s.db.QueryContext(ctx, `SELECT deal_id, n, kind, record_digest, local FROM deal_steps ORDER BY deal_id, n`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return list, nil
		}
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var id, prev string
	var events []sealedEvent
	flush := func() {
		if len(events) > 0 {
			list = append(list, dealListingOf(id, events))
		}
	}
	for rows.Next() {
		var rowID, kind, digest, local string
		var n int64
		if err = rows.Scan(&rowID, &n, &kind, &digest, &local); err != nil {
			return nil, err
		}
		if rowID != id {
			flush()
			id, prev, events = rowID, "", nil
		}
		var ev dealEvent
		if err = decodeJSON([]byte(local), &ev); err != nil {
			return nil, ErrConflict
		}
		if ev.DealID != id || ev.N != n || n != int64(len(events)+1) || ev.Kind != kind || ev.Prev != prev {
			return nil, ErrConflict
		}
		events = append(events, sealedEvent{Event: ev})
		prev = digest
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return list, nil
}

func dealListingOf(dealID string, events []sealedEvent) dealListing {
	l := dealListing{
		DealID: dealID, Kind: "deal", State: "open",
		OpenedAt: events[0].Event.At, LastRecordAt: events[len(events)-1].Event.At,
		LogID: dealLogID(dealID),
	}
	if saleIDPattern.MatchString(dealID) {
		l.Kind = "sale"
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
