package plugin

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// dohStub is a DoH endpoint (RFC 8484) answering TXT queries from records.
// answer, when set, replaces the reply to every query.
type dohStub struct {
	records  map[string][]string // name -> TXT records, each one string
	cnames   map[string]string   // name -> target
	ad       bool
	unsigned map[string]bool // names answered without AD, as "name."
	answer   func(w http.ResponseWriter, q dnsmessage.Message)
	queries  atomic.Int32
	srv      *httptest.Server
}

func (d *dohStub) url() string { return d.srv.URL + "/dns-query" }

func newDoHStub(t *testing.T, records map[string]string) *dohStub {
	t.Helper()
	d := &dohStub{records: map[string][]string{}, cnames: map[string]string{}, ad: true}
	for name, txt := range records {
		d.records[name] = []string{txt}
	}
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.queries.Add(1)
		body, _ := io.ReadAll(r.Body)
		var q dnsmessage.Message
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" || q.Unpack(body) != nil || len(q.Questions) != 1 {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		if len(q.Additionals) != 1 || !q.Additionals[0].Header.DNSSECAllowed() || q.CheckingDisabled {
			http.Error(w, "the query must ask for DNSSEC, with checking on", http.StatusBadRequest)
			return
		}
		if d.answer != nil {
			d.answer(w, q)
			return
		}
		d.reply(w, q, d.answerFor(q.Questions[0]))
	}))
	t.Cleanup(d.srv.Close)
	t.Setenv(dohEnv, d.url())
	old := dohClient
	client := d.srv.Client()
	client.CheckRedirect = old.CheckRedirect
	dohClient = client
	t.Cleanup(func() { dohClient = old })
	return d
}

// answerFor follows the stub's CNAMEs to TXT records, as a recursive
// resolver does.
func (d *dohStub) answerFor(q dnsmessage.Question) dnsmessage.Message {
	m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, RecursionAvailable: true, AuthenticData: d.ad && !d.unsigned[strings.ToLower(q.Name.String())]}, Questions: []dnsmessage.Question{q}}
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	for hops := 0; hops < 8; hops++ {
		if target, ok := d.cnames[name]; ok {
			m.Answers = append(m.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target + ".")},
			})
			name = target
			continue
		}
		break
	}
	txt, ok := d.records[name]
	if !ok && len(m.Answers) == 0 {
		m.RCode = dnsmessage.RCodeNameError
	}
	for _, t := range txt {
		// The strings of one record, in 255-octet pieces as on the wire.
		var parts []string
		for len(t) > 255 {
			parts, t = append(parts, t[:255]), t[255:]
		}
		parts = append(parts, t)
		m.Answers = append(m.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET},
			Body:   &dnsmessage.TXTResource{TXT: parts},
		})
	}
	return m
}

func (d *dohStub) reply(w http.ResponseWriter, _ dnsmessage.Message, m dnsmessage.Message) {
	b, err := m.Pack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	_, _ = w.Write(b)
}

// proxiedResolver makes the system resolver a fake-IP proxy: the From domain
// resolves to a placeholder address, and TXT queries pass through to records
// (none, for a proxy that answers NXDOMAIN). It returns how many TXT lookups
// reached it.
func proxiedResolver(t *testing.T, addr string, records map[string]string) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	oldProbe, oldLookup := systemProbe, dkimLookup
	systemProbe = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP(addr)}, nil }
	dkimLookup = func(name string) ([]string, error) {
		asked.Add(1)
		if txt, ok := records[name]; ok {
			return []string{txt}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	t.Cleanup(func() { systemProbe, dkimLookup = oldProbe, oldLookup })
	return &asked
}

func merchantRecords(t *testing.T) map[string]string {
	return map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)}
}

// On a host whose resolver hands out RFC 2544 placeholder addresses, the
// email is sealed at once from DoH, not deferred, and the record says so; the
// endpoint stays on the device.
func TestMerchantEmailKeyIsReadOverDoHWhenTheResolverIsProxied(t *testing.T) {
	dealFixture(t)
	doh := newDoHStub(t, merchantRecords(t))
	id := openMerchantDeal(t)
	system := proxiedResolver(t, "198.18.0.7", nil)

	sealed := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	assert.Equal(t, "pass", sealed["dkim"])
	assert.Equal(t, true, sealed["merchant_signed"])
	assert.Equal(t, "dns", sealed["key_source"], "the key is what the domain published in DNS, whatever carried it")
	assert.Equal(t, "doh", sealed["key_resolver"])
	assert.Equal(t, "resolver_validated", sealed["key_dnssec"])
	assert.Equal(t, doh.url(), sealed["key_resolver_endpoint"])
	assert.Equal(t, "reject", sealed["dmarc_policy"], "the DMARC records came over DoH too")
	assert.Positive(t, doh.queries.Load())
	assert.Zero(t, system.Load(), "a proxied resolver is asked only when DoH cannot be reached")

	record := dealRecords(t, id)[sealed["capsule_id"].(string)]
	private := dealPrivateValues(chainSteps(t, id))
	assert.True(t, dealRecordShareable(record, "", cli.DealAudienceCounterparty, private), "the new fields do not withhold the record from a shared copy")

	body := sealedEvidenceBody(t, id, sealed["capsule_id"].(string))
	me := body["merchant_email"].(map[string]any)
	assert.Equal(t, "doh", me["key_resolver"])
	assert.Equal(t, "resolver_validated", me["key_dnssec"])
	assert.NotContains(t, me, "key_resolver_endpoint")
	assert.Equal(t, true, body["verified"])

	// Verified offline, against the sealed key.
	stubDNS(t, nil)()
	v := dealRun(t, "verify-email", "--deal", id)
	assert.Equal(t, true, v["verified"])
}

// An IPv6 unique local address is a placeholder too.
func TestAnIPv6PlaceholderSkipsTheSystemResolver(t *testing.T) {
	newDoHStub(t, merchantRecords(t))
	proxiedResolver(t, "fdfe:dcba:9876::1", nil)
	e, err := captureEmail(readMerchantFixture(t), nil)
	require.NoError(t, err)
	assert.Equal(t, "doh", e.Resolver)
}

// A system resolver that works is used, and no DoH endpoint is contacted.
func TestNoDoHWhenTheSystemResolverWorks(t *testing.T) {
	dealFixture(t)
	doh := newDoHStub(t, merchantRecords(t))
	stubDNS(t, merchantRecords(t))
	id := openMerchantDeal(t)
	sealed := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	assert.Equal(t, "pass", sealed["dkim"])
	assert.Equal(t, "system", sealed["key_resolver"])
	assert.Equal(t, "not_validated", sealed["key_dnssec"], "the system resolver never vouches for DNSSEC")
	assert.NotContains(t, sealed, "key_resolver_endpoint")
	assert.Zero(t, doh.queries.Load(), "DoH is a fallback only")
	me := sealedEvidenceBody(t, id, sealed["capsule_id"].(string))["merchant_email"].(map[string]any)
	assert.Equal(t, "system", me["key_resolver"])
	assert.Equal(t, "not_validated", me["key_dnssec"])
}

// A system resolver that cannot be reached falls back to DoH in the same
// call; an answer without AD is sealed and says it was not validated.
func TestDoHIsUsedWhenTheSystemResolverFails(t *testing.T) {
	doh := newDoHStub(t, merchantRecords(t))
	doh.ad = false
	var systemAsked atomic.Int32
	old := dkimLookup
	dkimLookup = func(name string) ([]string, error) {
		systemAsked.Add(1)
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
	}
	t.Cleanup(func() { dkimLookup = old })
	e, err := captureEmail(readMerchantFixture(t), nil)
	require.NoError(t, err)
	assert.Equal(t, "pass", e.DKIM.Result)
	assert.Equal(t, "doh", e.Resolver)
	assert.Equal(t, "not_validated", e.DNSSEC)
	assert.EqualValues(t, 1, systemAsked.Load(), "a resolver that failed once is not waited on again")
}

// A proxy may answer NXDOMAIN for a key it will not resolve: a key the
// system resolver finds absent is checked over DoH, and DoH's answer sealed.
func TestAKeyTheSystemResolverFindsAbsentIsCheckedOverDoH(t *testing.T) {
	newDoHStub(t, merchantRecords(t))
	stubDNS(t, map[string]string{dmarcName: merchantDMARC(t)})
	e, err := captureEmail(readMerchantFixture(t), nil)
	require.NoError(t, err)
	assert.Equal(t, "pass", e.DKIM.Result)
	assert.Equal(t, "doh", e.Resolver)
}

// When DoH cannot be reached either, the system resolver's absent key stands:
// a domain that publishes no key is not deferred forever.
func TestAnAbsentKeyStandsWhenDoHCannotBeReached(t *testing.T) {
	doh := newDoHStub(t, nil)
	doh.srv.Close()
	stubDNS(t, map[string]string{dmarcName: merchantDMARC(t)})
	e, err := captureEmail(readMerchantFixture(t), nil)
	require.NoError(t, err)
	assert.Equal(t, "fail", e.DKIM.Result)
	assert.Equal(t, "system", e.Resolver)
	assert.Contains(t, e.keyCheck, "DNS-over-HTTPS could not be reached to check it", "the unchecked absence is said, not silent")
}

// deal note says so too.
func TestDealNoteSaysAnAbsentKeyWentUnchecked(t *testing.T) {
	dealFixture(t)
	id := openMerchantDeal(t)
	doh := newDoHStub(t, nil)
	doh.srv.Close()
	stubDNS(t, map[string]string{dmarcName: merchantDMARC(t)})
	got := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	assert.Equal(t, "fail", got["dkim"])
	assert.Equal(t, "system", got["key_resolver"])
	assert.Contains(t, got["key_check"], "DNS-over-HTTPS could not be reached to check it")
}

// One answer without AD makes the whole capture not_validated.
func TestOneUnvalidatedAnswerMakesTheCaptureNotValidated(t *testing.T) {
	doh := newDoHStub(t, merchantRecords(t))
	doh.unsigned = map[string]bool{dmarcName + ".": true}
	proxiedResolver(t, "198.18.0.7", nil)
	e, err := captureEmail(readMerchantFixture(t), nil)
	require.NoError(t, err)
	assert.Equal(t, "doh", e.Resolver)
	assert.Equal(t, "not_validated", e.DNSSEC)
}

// No resolver at all: the capture is deferred, with the reason, and the next
// tick lists it as waiting with its reason.
func TestDeferredWhenNeitherDNSNorDoHAnswers(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, merchantRecords(t))
	id := openMerchantDeal(t)
	doh := newDoHStub(t, nil)
	doh.answer = func(w http.ResponseWriter, q dnsmessage.Message) {
		doh.reply(w, q, dnsmessage.Message{Header: dnsmessage.Header{Response: true, RecursionAvailable: true, RCode: dnsmessage.RCodeServerFailure}, Questions: q.Questions})
	}
	blockDNS(t)
	got := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	assert.Equal(t, "deferred", got["state"])
	reason := got["reason"].(string)
	assert.Contains(t, reason, "through the system resolver (")
	assert.Contains(t, reason, "through DNS-over-HTTPS "+doh.url()+" (the resolver answered RCodeServerFailure)")
	assert.NotContains(t, reason, "\n")

	waiting := dealRun(t, "tick")["merchant_emails"].(map[string]any)["waiting"].([]any)
	require.Len(t, waiting, 1)
	assert.Contains(t, waiting[0].(map[string]any)["reason"], "DNS-over-HTTPS")

	// With DoH turned off, the reason says so.
	t.Setenv(dohEnv, "off")
	waiting = dealRun(t, "tick")["merchant_emails"].(map[string]any)["waiting"].([]any)
	assert.Contains(t, waiting[0].(map[string]any)["reason"], "DNS-over-HTTPS is turned off (CAPSULECTL_DOH=off)")
}

// A proxied resolver with DoH unreachable is deferred, never sealed from the
// proxy's answers.
func TestAProxiedResolverWithNoDoHDefers(t *testing.T) {
	doh := newDoHStub(t, nil)
	doh.srv.Close()
	proxiedResolver(t, "198.19.255.1", map[string]string{dmarcName: merchantDMARC(t)})
	_, err := captureEmail(readMerchantFixture(t), nil)
	var unreachable *keysUnreachableError
	require.ErrorAs(t, err, &unreachable)
	assert.Contains(t, err.Error(), "the system resolver is proxied: it answers shop.example with the placeholder address 198.19.255.1")
	assert.Contains(t, err.Error(), "the proxied system resolver found no DKIM key record, which proves nothing")
}

// A proxy that passes TXT queries through still seals, from the system
// resolver, when DoH is off or blocked and every key was found.
func TestAProxiedResolverThatPassesTXTThroughIsTheLastResort(t *testing.T) {
	t.Setenv(dohEnv, "off")
	asked := proxiedResolver(t, "198.18.0.9", merchantRecords(t))
	e, err := captureEmail(readMerchantFixture(t), nil)
	require.NoError(t, err)
	assert.Equal(t, "pass", e.DKIM.Result)
	assert.Equal(t, "system", e.Resolver)
	assert.Equal(t, "not_validated", e.DNSSEC)
	assert.Positive(t, asked.Load())
}

// The first endpoint that answers wrongly is passed over for the next.
func TestABadDoHEndpointMovesOnToTheNext(t *testing.T) {
	good := newDoHStub(t, merchantRecords(t))
	bad := newDoHStub(t, nil)
	bad.answer = func(w http.ResponseWriter, _ dnsmessage.Message) { http.Error(w, "nope", http.StatusBadGateway) }
	// httptest serves every stub under one certificate: one client trusts both.
	t.Setenv(dohEnv, bad.url()+", "+good.url())
	proxiedResolver(t, "198.18.1.1", nil)
	e, err := captureEmail(readMerchantFixture(t), nil)
	require.NoError(t, err)
	assert.Equal(t, good.url(), e.ResolverEndpoint)
	assert.EqualValues(t, 1, bad.queries.Load())
}

func TestDoHLookupRejectsAnswersItCannotTrust(t *testing.T) {
	const name = "sel._domainkey.shop.example"
	msg := func(q dnsmessage.Message, edit func(*dnsmessage.Message)) dnsmessage.Message {
		m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, RecursionAvailable: true}, Questions: q.Questions}
		edit(&m)
		return m
	}
	for label, c := range map[string]struct {
		answer func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message)
		want   string
	}{
		"http status": {func(_ *dohStub, w http.ResponseWriter, _ dnsmessage.Message) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		}, "HTTP 503"},
		"content type": {func(_ *dohStub, w http.ResponseWriter, _ dnsmessage.Message) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>captive portal</html>"))
		}, `answered "text/html", not a DNS message`},
		"malformed": {func(_ *dohStub, w http.ResponseWriter, _ dnsmessage.Message) {
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write([]byte{1, 2, 3})
		}, "malformed DNS answer"},
		"another question": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) {
				m.Questions = []dnsmessage.Question{{Name: dnsmessage.MustNewName("other.example."), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}}
			}))
		}, "not for the question asked"},
		"another id": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) { m.ID = 7 }))
		}, "not for the question asked"},
		"not a response": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) { m.Response = false }))
		}, "not for the question asked"},
		"truncated": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) { m.Truncated = true }))
		}, "truncated"},
		"not recursive": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) { m.RecursionAvailable = false }))
		}, "does not resolve recursively"},
		"extended rcode": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			// BADVERS (16): the header's four bits read NOERROR.
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) {
				var opt dnsmessage.ResourceHeader
				require.NoError(t, opt.SetEDNS0(1232, dnsmessage.RCode(16), false))
				m.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
			}))
		}, "the resolver answered 16"},
		"edns version": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) {
				var opt dnsmessage.ResourceHeader
				require.NoError(t, opt.SetEDNS0(1232, dnsmessage.RCode(16), false))
				opt.TTL |= 1 << 16
				m.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
			}))
		}, "EDNS record is malformed"},
		"two opt records": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) {
				var opt dnsmessage.ResourceHeader
				require.NoError(t, opt.SetEDNS0(1232, dnsmessage.RCodeSuccess, false))
				m.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}, {Header: opt, Body: &dnsmessage.OPTResource{}}}
			}))
		}, "EDNS record is malformed"},
		"refused": {func(d *dohStub, w http.ResponseWriter, q dnsmessage.Message) {
			d.reply(w, q, msg(q, func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeRefused }))
		}, "RCodeRefused"},
		"redirect": {func(_ *dohStub, w http.ResponseWriter, _ dnsmessage.Message) {
			w.Header().Set("Location", "http://elsewhere.example/dns-query")
			w.WriteHeader(http.StatusFound)
		}, "HTTP 302"},
		"too large": {func(_ *dohStub, w http.ResponseWriter, _ dnsmessage.Message) {
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(make([]byte, maxDoHResponse+1))
		}, "larger than a DNS message"},
	} {
		t.Run(label, func(t *testing.T) {
			d := newDoHStub(t, nil)
			d.answer = func(w http.ResponseWriter, q dnsmessage.Message) { c.answer(d, w, q) }
			_, validated, err := dohLookupTXT(d.url(), name)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
			assert.False(t, validated)
			var dnsErr *net.DNSError
			assert.False(t, errors.As(err, &dnsErr) && dnsErr.IsNotFound, "an answer it cannot trust is never read as an absent record")
		})
	}
}

func TestDoHLookupReadsRecordsAsTheSystemResolverDoes(t *testing.T) {
	long := strings.Repeat("k", 600)
	d := newDoHStub(t, nil)
	d.records["sel._domainkey.shop.example"] = []string{"v=DKIM1; p=" + long}
	d.records["two.example"] = []string{"first", "second"}
	d.cnames["cn._domainkey.shop.example"] = "cn.dkim.provider.example"
	d.records["cn.dkim.provider.example"] = []string{"v=DKIM1; p=abc"}
	d.cnames["dangling.example"] = "nowhere.example"

	txt, validated, err := dohLookupTXT(d.url(), "sel._domainkey.shop.example.")
	require.NoError(t, err)
	assert.Equal(t, []string{"v=DKIM1; p=" + long}, txt, "the strings of one record are joined")
	assert.True(t, validated)

	txt, _, err = dohLookupTXT(d.url(), "two.example")
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, txt, "separate records stay separate")

	txt, _, err = dohLookupTXT(d.url(), "CN._domainkey.shop.example")
	require.NoError(t, err)
	assert.Equal(t, []string{"v=DKIM1; p=abc"}, txt, "a CNAME chain is followed")

	// A name no resolver would look up is absent, as the system resolver
	// says, never an unreachable resolver that defers the email forever.
	for _, name := range []string{"absent.example", "dangling.example", strings.Repeat("x", 64) + "._domainkey.shop.example", "a..example"} {
		_, _, err = dohLookupTXT(d.url(), name)
		var dnsErr *net.DNSError
		require.ErrorAs(t, err, &dnsErr, name)
		assert.True(t, dnsErr.IsNotFound, name)
	}
}

func TestDoHEndpointsConfig(t *testing.T) {
	t.Setenv(dohEnv, "")
	got, err := dohEndpoints()
	require.NoError(t, err)
	assert.Equal(t, dohDefaultEndpoints, got)
	assert.Contains(t, got, "https://1.1.1.1/dns-query", "an endpoint by address, for a resolver that resolves nothing")

	t.Setenv(dohEnv, " https://a.example/dns-query , https://b.example/q ")
	got, err = dohEndpoints()
	require.NoError(t, err)
	assert.Equal(t, []string{"https://a.example/dns-query", "https://b.example/q"}, got)

	t.Setenv(dohEnv, "OFF")
	_, err = dohEndpoints()
	assert.EqualError(t, err, "DNS-over-HTTPS is turned off (CAPSULECTL_DOH=off)")

	for _, bad := range []string{"http://a.example/dns-query", "a.example", "https://user:secret@a.example/q", "https://a.example/q,"} {
		t.Setenv(dohEnv, bad)
		_, err = dohEndpoints()
		require.Error(t, err, bad)
		assert.NotContains(t, err.Error(), "secret")
	}

	// A malformed setting defers the capture with the reason; it never
	// drops the email.
	t.Setenv(dohEnv, "http://a.example")
	blockDNS(t)
	_, err = captureEmail(readMerchantFixture(t), nil)
	var unreachable *keysUnreachableError
	require.ErrorAs(t, err, &unreachable)
	assert.Contains(t, err.Error(), "CAPSULECTL_DOH must be a comma-separated list of https:// URLs, or off")
}

// A probe that fails or finds no address says nothing about a proxy.
func TestAProbeThatFindsNothingKeepsTheSystemResolver(t *testing.T) {
	t.Setenv(dohEnv, "https://doh.example/dns-query")
	old := systemProbe
	t.Cleanup(func() { systemProbe = old })
	for _, probe := range []func(string) ([]net.IP, error){
		func(string) ([]net.IP, error) { return nil, errors.New("no such host") },
		func(string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.9"), net.ParseIP("2001:db8::1")}, nil
		},
	} {
		systemProbe = probe
		list, skipped := keyResolvers("shop.example")
		require.NotEmpty(t, list)
		assert.Equal(t, resolverSystem, list[0].via)
		assert.NoError(t, skipped)
	}
}

// key_resolver and key_dnssec come as a pair, on a dns key only.
func TestMerchantEmailSchemaPairsResolverFields(t *testing.T) {
	s, err := compileDealSchemaAt("#/$defs/merchant_email")
	require.NoError(t, err)
	base := func() map[string]any {
		return map[string]any{
			"message_digest": strings.Repeat("ab", 32), "key_records_digest": strings.Repeat("cd", 32), "key_source": "dns",
			"dkim": "pass", "merchant_signed": true, "dmarc_policy": "reject", "parsed": map[string]any{"method": "heuristic"},
		}
	}
	assert.NoError(t, s.Validate(base()), "a record sealed before the fields existed")
	ok := base()
	ok["key_resolver"], ok["key_dnssec"] = "doh", "resolver_validated"
	assert.NoError(t, s.Validate(ok))
	for label, edit := range map[string]func(map[string]any){
		"resolver alone": func(m map[string]any) { m["key_resolver"] = "system" },
		"dnssec alone":   func(m map[string]any) { m["key_dnssec"] = "not_validated" },
		"on a supplied key": func(m map[string]any) {
			m["key_source"], m["key_resolver"], m["key_dnssec"] = "supplied", "doh", "resolver_validated"
		},
		"unknown resolver": func(m map[string]any) {
			m["key_resolver"], m["key_dnssec"] = "https://doh.example", "resolver_validated"
		},
	} {
		m := base()
		edit(m)
		assert.Error(t, s.Validate(m), label)
	}
}

func readMerchantFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(merchantFixture, "confirmation.eml"))
	require.NoError(t, err)
	return raw
}
