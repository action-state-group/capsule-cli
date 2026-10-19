package plugin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

const dealTermsResultLong = `Reconcile your final close of a deal with the counterparty's, and write the
result as an Evidence Result with one claim per requirement of the Deal
Reconcile Contract, ec:deal-reconcile@1: whether both closes commit to the
same amount, and to the same currency.

The counterparty's close is read from the copy of the deal you recorded with
` + "`deal request --response`" + ` (--response names that record). Its amount and
currency are opened with the openings in the counterparty's own
` + "`disclose --deal ID --share counterparty`" + ` file (--openings). Only those
openings are read from that file. Without it the counterparty's close cannot
be compared, and both claims are not evaluable.

The period defaults to the two closes' recorded times. --from and --to set it
instead, as [from, to) in RFC 3339 UTC seconds.

--check FILE checks a Result under ec:deal-reconcile@1 against the inputs
given, and writes nothing. The Result names its two closes by digest, and the
closes read here must be exactly those. It does not record which openings were
carried, or a coverage declaration (none is given here), so the check uses the
openings named: when the Result names your key as its peer (the counterparty
made it), pass --sent with the disclose file you handed them, and your openings
are read from that file only. "differs" means the Result is not the recompute
of these inputs. It is not a finding that a claim is false. "cannot_reproduce"
means the inputs needed are not held, and says nothing about the claims. An
adjudicator's copy carries no openings, so an adjudicator cannot check one.`

func dealTermsResultCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "terms-result", Short: "Reconcile your final close of a deal with the counterparty's and write the Evidence Result (ec:deal-reconcile@1), or --check one", Long: dealTermsResultLong, Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		response, _ := c.Flags().GetString("response")
		openingsPath, _ := c.Flags().GetString("openings")
		from, _ := c.Flags().GetString("from")
		to, _ := c.Flags().GetString("to")
		out, _ := c.Flags().GetString("out")
		check, _ := c.Flags().GetString("check")
		sent, _ := c.Flags().GetString("sent")
		if response == "" {
			return cli.InputError("--response is required: the record `deal request --response` printed for the counterparty's copy")
		}
		if check != "" && (out != "" || from != "" || to != "") {
			return cli.InputError("--check takes the period from the Result and writes nothing: drop --out, --from and --to")
		}
		if check == "" && (out == "" || sent != "") {
			return cli.InputError("--out is required, and --sent goes with --check only")
		}
		var openings map[string]map[string]evidencebook.Opening
		if openingsPath != "" {
			raw, err := cli.ReadInput(openingsPath)
			if err != nil {
				return err
			}
			if openings, err = dealOpeningsFile(raw); err != nil {
				return err
			}
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, _ []sealedEvent) error {
			if check != "" {
				return s.checkTermsResult(c, dealID, response, openings, check, sent)
			}
			return s.writeTermsResult(c, dealID, response, openings, from, to, out)
		})
	}}
	cmd.Flags().String("deal", "", "The deal id")
	cmd.Flags().String("response", "", "The record `deal request --response` printed for the counterparty's copy of the deal")
	cmd.Flags().String("openings", "", "The counterparty's `disclose --share counterparty` file: only its field_openings are read")
	cmd.Flags().String("from", "", "The period's start, RFC 3339 UTC seconds (default: the earlier close's recorded time)")
	cmd.Flags().String("to", "", "The period's end, exclusive (default: one second after the later close's recorded time)")
	cmd.Flags().String("out", "", "Where to write the Evidence Result")
	cmd.Flags().String("check", "", "Check this Evidence Result against the inputs given instead of writing one")
	cmd.Flags().String("sent", "", "With --check, when the counterparty made the Result: the `disclose --share counterparty` file you handed them")
	return cmd
}

// reconcileSide is one side's half, when it has one, and the openings the
// reconcile is given for it.
type reconcileSide struct {
	book     string
	half     *dealHalf
	openings map[string]evidencebook.Opening
}

// peerSide is the counterparty's side: the copy recorded for the response,
// and the half the openings name, admitted (note §2.10 Decision 2).
func (s *dealSession) peerSide(ctx context.Context, dealID, response string, openings map[string]map[string]evidencebook.Opening) (reconcileSide, error) {
	peer, err := s.peerCopy(ctx, dealID, response)
	if err != nil {
		return reconcileSide{}, err
	}
	side := reconcileSide{book: peer.pinned}
	digest, err := soleDigest(openings)
	if err != nil || digest == "" {
		return side, err
	}
	half, err := admitPeerHalf(dealID, peer, digest, openings[digest])
	if err != nil {
		return reconcileSide{}, err
	}
	side.half, side.openings = &half, openings[digest]
	return side, nil
}

// ownKey is this profile's signing key, as the hex a half's key_id carries.
func (s *dealSession) ownKey() string {
	return hex.EncodeToString(s.key.Public().(ed25519.PublicKey))
}

// recompute runs evidencebook's recompute over the two sides.
func recompute(a, b reconcileSide, period evidencebook.ResultPeriod, generatedAt string) (evidencebook.DealReconcile, error) {
	in := evidencebook.DealReconcileInput{
		SideA: a.book, Peer: b.book, Period: period, GeneratedAt: generatedAt,
		HalfMappings:  map[string]evidencebook.HalfMapping{a.book: dealHalfMapping, b.book: dealHalfMapping},
		FieldOpenings: map[string]map[string]evidencebook.Opening{},
	}
	for _, side := range []reconcileSide{a, b} {
		if side.half == nil {
			continue
		}
		in.Halves = append(in.Halves, evidencebook.DealHalf{Book: side.book, Octets: side.half.octets})
		if len(side.openings) > 0 {
			in.FieldOpenings[side.half.digest] = side.openings
		}
	}
	return evidencebook.RecomputeDealReconcile(in)
}

const resultTimeLayout = "2006-01-02T15:04:05Z"

// reconcilePeriod is the period given, or the default: from the earlier
// half's recorded time to one second after the later one's.
func reconcilePeriod(from, to string, sides ...reconcileSide) (evidencebook.ResultPeriod, error) {
	var times []time.Time
	for _, side := range sides {
		if side.half != nil {
			times = append(times, side.half.recordedAt.UTC())
		}
	}
	if len(times) == 0 {
		return evidencebook.ResultPeriod{}, cli.InputError("nothing to reconcile: the deal has no final close here, and the counterparty's openings name none")
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	period := evidencebook.ResultPeriod{Start: times[0].Format(resultTimeLayout), End: times[len(times)-1].Add(time.Second).Format(resultTimeLayout)}
	for _, bound := range []struct {
		flag, value string
		into        *string
	}{{"--from", from, &period.Start}, {"--to", to, &period.End}} {
		if bound.value == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, bound.value)
		if err != nil || t.UTC().Format(resultTimeLayout) != bound.value {
			return evidencebook.ResultPeriod{}, cli.InputError(bound.flag + " is an RFC 3339 UTC time to the second, such as 2026-10-19T00:00:00Z")
		}
		*bound.into = bound.value
	}
	start, _ := time.Parse(time.RFC3339, period.Start)
	end, _ := time.Parse(time.RFC3339, period.End)
	if !start.Before(end) {
		return evidencebook.ResultPeriod{}, cli.InputError("the period's start must be before its end")
	}
	inPeriod := 0
	for _, t := range times {
		if !t.Before(start) && t.Before(end) {
			inPeriod++
		}
	}
	if inPeriod == 0 {
		return evidencebook.ResultPeriod{}, cli.InputError("nothing to reconcile in this period: neither close was recorded in it")
	}
	return period, nil
}

// writeTermsResult is the verb: this profile is side A, the counterparty
// side B.
func (s *dealSession) writeTermsResult(c *cobra.Command, dealID, response string, openings map[string]map[string]evidencebook.Opening, from, to, out string) error {
	ctx := c.Context()
	own, ownOpenings, err := s.ownDealHalf(ctx, dealID)
	if err != nil {
		return err
	}
	peer, err := s.peerSide(ctx, dealID, response, openings)
	if err != nil {
		return err
	}
	a := reconcileSide{book: s.ownKey(), half: own, openings: ownOpenings}
	period, err := reconcilePeriod(from, to, a, peer)
	if err != nil {
		return err
	}
	got, err := recompute(a, peer, period, cli.DealClock().UTC().Format(resultTimeLayout))
	if err != nil {
		return cli.Hint(cli.ErrConflict, "the reconcile could not be recomputed: "+err.Error())
	}
	encoded, err := json.MarshalIndent(got.Result, "", "  ")
	if err != nil {
		return err
	}
	if err = cli.AtomicFile(out, append(encoded, '\n'), false); err != nil {
		return err
	}
	return cli.Output(c, termsSummary(dealID, out, got.Result))
}

// termsSummary is what the verb prints: the claims' verdicts and
// tallies. The contract's optional per-deal state is never shown.
func termsSummary(dealID, out string, result evidencebook.DealReconcileResult) map[string]any {
	claims := make([]map[string]any, 0, len(result.Claims))
	for _, claim := range result.Claims {
		claims = append(claims, map[string]any{
			"requirement_ref": claim.RequirementRef, "verdict": claim.Verdict, "sufficiency": claim.Sufficiency, "tallies": claim.Reconcile.Tallies,
		})
	}
	period := evidencebook.ResultPeriod{}
	if len(result.Claims) > 0 {
		period = result.Claims[0].Reconcile.Period
	}
	return map[string]any{"deal_id": dealID, "contract_ref": evidencebook.DealReconcileContractRef, "period": period, "claims": claims, "out": out}
}

// Outcomes of --check.
const (
	termsCheckEqual           = "equal"
	termsCheckDiffers         = "differs"
	termsCheckCannotReproduce = "cannot_reproduce"
)

// checkTermsResult checks a Result against the inputs given (note
// §2.10 Decision 6). The Result names its halves: each claim's evidence is
// the sorted digests of every half in the reconcile (DR §7.3), and the
// halves read here must be exactly those. It records neither which openings
// were carried nor a coverage declaration (DR §7.3, DR-3), so the check
// recomputes from exactly the openings named: the counterparty's own
// openings come from the file it sent (--sent), never from its store. No
// coverage declaration is given, as the verb gives none.
func (s *dealSession) checkTermsResult(c *cobra.Command, dealID, response string, openings map[string]map[string]evidencebook.Opening, checkPath, sentPath string) error {
	ctx := c.Context()
	report := func(outcome, detail string) error {
		out := map[string]any{"deal_id": dealID, "check": outcome, "detail": detail}
		if err := cli.Output(c, out); err != nil {
			return err
		}
		switch outcome {
		case termsCheckDiffers:
			return cli.Hint(cli.ErrConflict, "the Result is not the recompute of the inputs given: "+detail)
		case termsCheckCannotReproduce:
			return cli.Hint(cli.ErrInput, "cannot reproduce the Result's inputs: "+detail)
		}
		return nil
	}
	raw, err := cli.ReadInput(checkPath)
	if err != nil {
		return err
	}
	doc, err := decodeResultDocument(raw)
	if err != nil {
		return err
	}
	var result evidencebook.DealReconcileResult
	if err = json.Unmarshal(raw, &result); err != nil || len(result.Claims) == 0 {
		return report(termsCheckDiffers, "the Result holds no reconcile claims")
	}
	for _, claim := range result.Claims {
		if claim.ContractRef != evidencebook.DealReconcileContractRef {
			return report(termsCheckDiffers, "claim "+claim.ID+" is not under "+evidencebook.DealReconcileContractRef)
		}
		if claim.Reconcile.Peer != result.Claims[0].Reconcile.Peer || claim.Reconcile.Period != result.Claims[0].Reconcile.Period {
			return report(termsCheckDiffers, "the claims name different peers or periods")
		}
	}
	peerKey, period := result.Claims[0].Reconcile.Peer, result.Claims[0].Reconcile.Period
	own, ownOpenings, err := s.ownDealHalf(ctx, dealID)
	if err != nil {
		return err
	}
	other, err := s.peerSide(ctx, dealID, response, openings)
	if err != nil {
		return err
	}
	mine := reconcileSide{book: s.ownKey(), half: own, openings: ownOpenings}
	a, b := mine, other
	inputs := "this profile's openings and the counterparty's openings file"
	if peerKey == mine.book {
		// The counterparty made the Result: it is side A, and this profile's
		// openings are the ones the file it was handed carried.
		if sentPath == "" {
			return report(termsCheckCannotReproduce, "the Result names this profile as its peer: pass --sent, the disclose file you handed the counterparty")
		}
		sent, err := os.ReadFile(sentPath)
		if err != nil {
			return cli.InputError("cannot read --sent: " + err.Error())
		}
		recorded, err := s.recordedCounterpartyShare(ctx, dealID, cli.BytesDigest(sent))
		if err != nil {
			return err
		}
		if !recorded {
			return report(termsCheckCannotReproduce, "--sent is no `disclose --share counterparty` file this profile recorded for the deal")
		}
		sentOpenings, err := dealOpeningsFile(sent)
		if err != nil {
			return err
		}
		mine.openings = nil
		if own != nil {
			mine.openings = sentOpenings[own.digest]
		}
		// The maker held every opening of its own close, so its file must
		// carry both for the check to have what it had.
		if other.half == nil || !opensEvery(other.openings) {
			return report(termsCheckCannotReproduce, "--openings, the counterparty's disclose file, does not carry both openings of their close")
		}
		a, b = other, mine
		inputs = "the counterparty's openings file and the file sent to them (" + cli.BytesDigest(sent) + ")"
	} else if peerKey != other.book {
		return report(termsCheckDiffers, "the Result's peer is not the responder of the copy recorded for --response")
	}
	if !sameHalves(result, period, a, b) {
		return report(termsCheckDiffers, "the Result's evidence (DR §7.3) names halves other than the closes read here")
	}
	got, err := recompute(a, b, period, result.GeneratedAt)
	if err != nil {
		return report(termsCheckDiffers, "the inputs do not recompute: "+err.Error())
	}
	if member := firstDifference(doc, got.Result); member != "" {
		return report(termsCheckDiffers, member+" differs from the recompute of "+inputs)
	}
	return report(termsCheckEqual, "the Result is the recompute of "+inputs)
}

// recordedCounterpartyShare is whether a counterparty share this profile
// recorded for the deal is of the file with this digest: a `disclose
// --share counterparty` file, whose bytes are the ones recordShare hashed. A
// respond copy names its request and is not one.
func (s *dealSession) recordedCounterpartyShare(ctx context.Context, dealID, digest string) (found bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM deal_disclosures WHERE deal_id=?`, dealID)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var record map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return false, cli.ErrConflict
		}
		_, answered := record["request_digest"]
		if record["audience"] == cli.DealAudienceCounterparty && record["receipt_sha256"] == digest && !answered {
			found = true
		}
	}
	return found, rows.Err()
}

// firstDifference names the first member of the Result that differs from
// the recompute, compared after JCS, or "" when none does.
func firstDifference(doc map[string]interface{}, want evidencebook.DealReconcileResult) string {
	encoded, err := json.Marshal(want)
	if err != nil {
		return "the Result"
	}
	expected, err := cli.DecodeBundleJSON(encoded)
	if err != nil {
		return "the Result"
	}
	same := func(a, b interface{}) bool {
		x, errA := canonical.JCS(a)
		y, errB := canonical.JCS(b)
		return errA == nil && errB == nil && bytes.Equal(x, y)
	}
	if same(doc, expected) {
		return ""
	}
	gotClaims, _ := doc["claims"].([]interface{})
	wantClaims, _ := expected["claims"].([]interface{})
	if len(gotClaims) != len(wantClaims) {
		return "the number of claims"
	}
	for i := range wantClaims {
		got, _ := gotClaims[i].(map[string]interface{})
		want, _ := wantClaims[i].(map[string]interface{})
		keys := map[string]bool{}
		for k := range got {
			keys[k] = true
		}
		for k := range want {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if !same(got[k], want[k]) {
				return fmt.Sprintf("claim %v's %s", want["id"], k)
			}
		}
	}
	for _, k := range []string{"aggregate", "result_version", "generated_at"} {
		if !same(doc[k], expected[k]) {
			return "the Result's " + k
		}
	}
	return "a member the Result adds"
}

// sameHalves is whether every claim's evidence is exactly the digests of the
// halves read here that lie in the period (DR §7.3).
func sameHalves(result evidencebook.DealReconcileResult, period evidencebook.ResultPeriod, sides ...reconcileSide) bool {
	start, errStart := time.Parse(time.RFC3339, period.Start)
	end, errEnd := time.Parse(time.RFC3339, period.End)
	if errStart != nil || errEnd != nil {
		return false
	}
	var want []string
	for _, side := range sides {
		if h := side.half; h != nil && !h.recordedAt.Before(start) && h.recordedAt.Before(end) {
			want = append(want, h.digest)
		}
	}
	sort.Strings(want)
	for _, claim := range result.Claims {
		got := make([]string, 0, len(claim.Evidence))
		for _, e := range claim.Evidence {
			got = append(got, e.Digest)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return false
		}
	}
	return true
}

// opensEvery is whether openings carry an opening for each compared field.
func opensEvery(openings map[string]evidencebook.Opening) bool {
	for _, f := range dealHalfFields {
		if _, ok := openings[f]; !ok {
			return false
		}
	}
	return true
}
