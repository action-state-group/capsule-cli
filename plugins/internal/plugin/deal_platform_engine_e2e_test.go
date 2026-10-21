package plugin

// Opt-in (E2E_OUT=dir): captures the external-check inputs capsulectl
// gives a rules checker for platform shares, to decide under capsule-engine
// with scripts/engine-e2e/decide_platform.py (capsule-cli#348).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapturePlatformCheckInputsForTheEngine(t *testing.T) {
	out := os.Getenv("E2E_OUT")
	if out == "" {
		t.Skip("E2E_OUT not set")
	}
	capture := func(name string, open func() string, check string) {
		dealFixture(t)
		checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
		pinChecker(t, map[string]any{"command": []string{checker}})
		id := open()
		res := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, check))
		raw, err := os.ReadFile(checker + ".input")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(out, name+".input.json"), raw, 0o644))
		t.Logf("%s: capsulectl verdict %v", name, res["verdict"])
	}
	buyer := func() string { return openSeller(t, `{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"}`) }
	seller := func() string {
		return dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"sell my bike for at least $100","party_role":"seller","min_total_minor":10000,"allowed":["offer","commit","share_contact"],"ordered_representations":[]},
		"who":{"name":"Buyer Bea","profile_id":"mkt:buyer:77"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
	}
	for _, c := range []struct{ name, classes string }{{"email", `"email"`}, {"phone", `"phone"`}, {"address", `"home_address"`}} {
		check := `{"action":"share_contact","disclosing_to":"platform","disclosing":[` + c.classes + `],"recipient":{"name":"Craigslist","domain":"craigslist.org"}}`
		capture("everyday-"+c.name, buyer, check)
		capture("seller-"+c.name, seller, check)
	}
}
