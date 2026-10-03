package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// An optional remote checker adds a second reading of a deal step. It is off
// unless the operator names one, it only ever adds differences (a remote
// "pass" never clears a local pause), and when it is slow or unreachable the
// local rules decide alone. It receives the minimal fields below and never
// message text, names, contact details or card numbers.
const (
	dealCheckURLEnv   = "CAPSULE_DEAL_CHECK_URL"
	dealCheckTokenEnv = "CAPSULE_DEAL_CHECK_TOKEN"
	dealRemoteTimeout = 2 * time.Second
	dealRemoteMaxBody = 64 << 10
)

type dealRemoteRequest struct {
	Spec          string   `json:"spec"`
	DealType      string   `json:"deal_type"`
	Action        string   `json:"action"`
	Domain        string   `json:"counterparty_domain,omitempty"`
	AmountMinor   *int64   `json:"amount_minor,omitempty"`
	Currency      string   `json:"currency,omitempty"`
	Rail          string   `json:"rail,omitempty"`
	Refundable    *bool    `json:"refundable,omitempty"`
	DomainAgeDays *int64   `json:"domain_age_days,omitempty"`
	LocalRules    []string `json:"local_rules"`
}

type dealRemoteResponse struct {
	Verdict     string `json:"verdict"`
	Differences []struct {
		Rule string `json:"rule"`
		Text string `json:"text"`
	} `json:"differences"`
}

// dealRemoteBase returns the configured checker base URL, or "" when none is
// configured. HTTPS is required except for a loopback host.
func dealRemoteBase() (string, error) {
	raw := strings.TrimRight(os.Getenv(dealCheckURLEnv), "/")
	if raw == "" {
		return "", nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", inputError(dealCheckURLEnv + " must be a URL without credentials or query")
	}
	loopback := false
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (loopback || u.Hostname() == "localhost")) {
		return "", inputError(dealCheckURLEnv + " must use https")
	}
	return raw, nil
}

func dealRemotePost(ctx context.Context, base, path string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dealRemoteTimeout)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv(dealCheckTokenEnv); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		return nil, e
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, dealRemoteMaxBody+1))
	e = errors.Join(e, resp.Body.Close())
	if e != nil {
		return nil, e
	}
	if resp.StatusCode != http.StatusOK || len(b) > dealRemoteMaxBody {
		return nil, errors.New("remote checker unavailable")
	}
	return b, nil
}

// dealRemoteWarm asks a configured checker to get ready while the deal is
// still being negotiated. Its result never affects the deal.
func dealRemoteWarm(ctx context.Context) string {
	base, e := dealRemoteBase()
	if e != nil || base == "" {
		return "not_configured"
	}
	if _, e = dealRemotePost(ctx, base, "/v1/warm", []byte(`{}`)); e != nil {
		return "unavailable"
	}
	return "sent"
}

func dealRemoteCheck(ctx context.Context, s dealState, snap dealSnapshot, local dealCheckResult) (dealRemoteResult, error) {
	base, e := dealRemoteBase()
	if e != nil {
		return dealRemoteResult{}, e
	}
	if base == "" {
		return dealRemoteResult{Status: "not_configured"}, nil
	}
	who := s.who
	if snap.Who != nil {
		who = overlayWho(who, *snap.Who)
	}
	terms := s.terms
	if snap.Terms != nil {
		terms = overlayTerms(terms, *snap.Terms)
	}
	recourse := s.recourse
	if snap.Recourse != nil {
		recourse = overlayRecourse(recourse, *snap.Recourse)
	}
	amount := snap.AmountMinor
	if amount == nil {
		amount = terms.PriceMinor
	}
	rules := []string{}
	for _, d := range local.Differences {
		if !slices.Contains(rules, d.Rule) {
			rules = append(rules, d.Rule)
		}
	}
	body, e := json.Marshal(dealRemoteRequest{
		Spec: "deal-check-request/v0", DealType: s.open.Type, Action: snap.Action, Domain: who.Domain,
		AmountMinor: amount, Currency: terms.Currency, Rail: normRail(recourse.Rail), Refundable: recourse.Refundable,
		DomainAgeDays: who.DomainAgeDays, LocalRules: rules,
	})
	if e != nil {
		return dealRemoteResult{}, e
	}
	raw, e := dealRemotePost(ctx, base, "/v1/check", body)
	if e != nil {
		return dealRemoteResult{Status: "unavailable"}, nil
	}
	var resp dealRemoteResponse
	if json.Unmarshal(raw, &resp) != nil || (resp.Verdict != "pass" && resp.Verdict != "pause") {
		return dealRemoteResult{Status: "unavailable"}, nil
	}
	sum := sha256.Sum256(raw)
	out := dealRemoteResult{Status: "used", ResponseSHA256: hex.EncodeToString(sum[:]), Differences: []dealDifference{}}
	for _, d := range resp.Differences {
		if strings.TrimSpace(d.Text) == "" {
			continue
		}
		out.Differences = append(out.Differences, dealDifference{Question: "remote", Rule: d.Rule, Text: d.Text})
	}
	if resp.Verdict == "pause" && len(out.Differences) == 0 {
		out.Differences = append(out.Differences, dealDifference{Question: "remote", Rule: "remote_pause", Text: "The second reader asked to pause"})
	}
	return out, nil
}
