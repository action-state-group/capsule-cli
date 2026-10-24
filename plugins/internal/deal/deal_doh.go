package deal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Some hosts reach the internet only through a proxy whose DNS hands out
// placeholder addresses (RFC 2544's 198.18.0.0/15) and routes the connection
// by name, so HTTPS works while the system resolver cannot answer a TXT
// query, or answers it falsely. A merchant's DKIM key has to be read soon
// after the email arrives, before the sender rotates it, so such a host reads
// it over DNS-over-HTTPS (RFC 8484) in the same call instead of deferring.
//
// A capture uses one resolver from start to finish, so a sealed record names
// exactly one source for all of its key and DMARC records: the system
// resolver, unless it is proxied or cannot be reached, then each DoH endpoint
// in turn. When none can be reached the capture is deferred, as on a host
// with no network at all.

// dohDefaultEndpoints are tried in order when CAPSULECTL_DOH is unset. All
// validate DNSSEC and say so with the AD bit. The last two are the same
// services by address (their certificates name the address), for a host
// whose resolver cannot even resolve the first two.
var dohDefaultEndpoints = []string{
	"https://cloudflare-dns.com/dns-query", "https://dns.google/dns-query",
	"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query",
}

// dohEnv names the DoH endpoints: a comma-separated list of https URLs, or
// "off" to never leave the system resolver.
const dohEnv = "CAPSULECTL_DOH"

const (
	resolverSystem = "system"
	resolverDoH    = "doh"
	dnssecValid    = "resolver_validated"
	dnssecNotValid = "not_validated"
)

// dohClient sends DoH queries; it honors the proxy environment and follows
// no redirect, so an answer always comes from the endpoint named.
var dohClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// systemProbe reads a domain's addresses from the system resolver, to tell
// whether it is a placeholder-handing proxy.
var systemProbe = func(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// placeholderNets are what fake-IP DNS proxies hand out for outside names:
// RFC 2544's benchmarking range, and IPv6 unique local addresses (RFC 4193).
// Neither is ever a public domain's real address.
var placeholderNets = []*net.IPNet{
	{IP: net.IPv4(198, 18, 0, 0).To4(), Mask: net.CIDRMask(15, 32)},
	{IP: net.ParseIP("fc00::"), Mask: net.CIDRMask(7, 128)},
}

func isPlaceholder(ip net.IP) bool {
	for _, n := range placeholderNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// keyResolver is one way of reading TXT records at seal time. lookup reports
// whether the answer was DNSSEC-validated by the resolver (the AD bit).
type keyResolver struct {
	via      string // resolverSystem or resolverDoH
	endpoint string // the DoH URL; empty for the system resolver
	// proxied marks the system resolver, tried last, on a host where it
	// hands out placeholder addresses: some such proxies pass TXT queries
	// through, but an absent key from one proves nothing.
	proxied bool
	lookup  func(name string) (txt []string, validated bool, err error)
}

func (r keyResolver) String() string {
	if r.via == resolverDoH {
		return "DNS-over-HTTPS " + r.endpoint
	}
	return "the system resolver"
}

var systemResolver = keyResolver{via: resolverSystem, lookup: func(name string) ([]string, bool, error) {
	// Go's resolver does not expose the AD bit: never claimed.
	txt, err := dkimLookup(name)
	return txt, false, err
}}

// keyResolvers is the order a capture tries resolvers in, and why any was
// left out. The system resolver goes last when the From domain resolves to a
// placeholder address through it; a probe that fails or finds nothing says
// nothing either way.
func keyResolvers(from string) (list []keyResolver, skipped error) {
	if from != "" {
		ips, _ := systemProbe(from)
		for _, ip := range ips {
			if isPlaceholder(ip) {
				skipped = fmt.Errorf("the system resolver is proxied: it answers %s with the placeholder address %s", from, ip)
				break
			}
		}
	}
	if skipped == nil {
		list = append(list, systemResolver)
	}
	endpoints, err := dohEndpoints()
	for _, ep := range endpoints {
		list = append(list, keyResolver{via: resolverDoH, endpoint: ep, lookup: func(name string) ([]string, bool, error) {
			return dohLookupTXT(ep, name)
		}})
	}
	if skipped != nil {
		last := systemResolver
		last.proxied = true
		list = append(list, last)
	}
	return list, errors.Join(skipped, err)
}

// dohEndpoints reads CAPSULECTL_DOH. A malformed value is a reason the
// capture could not use DoH, never a reason to drop the email.
func dohEndpoints() ([]string, error) {
	v := strings.TrimSpace(os.Getenv(dohEnv))
	switch {
	case v == "":
		return dohDefaultEndpoints, nil
	case strings.EqualFold(v, "off"):
		return nil, errors.New("DNS-over-HTTPS is turned off (" + dohEnv + "=off)")
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, errors.New(dohEnv + " must be a comma-separated list of https:// URLs, or off")
		}
		out = append(out, s)
	}
	return out, nil
}

// maxDoHResponse bounds a DoH answer; a DNS message is at most 64 KiB.
const maxDoHResponse = 64 << 10

// dohLookupTXT asks one DoH endpoint (RFC 8484, POST) for name's TXT records,
// with the DNSSEC OK bit set and checking left on, following a CNAME chain in
// the answer as the system resolver does (a recursive resolver chases the
// chain, so a chain that ends with no TXT is a target with no record).
// validated is the resolver's own report (AD) that it checked DNSSEC. As
// net.Resolver.LookupTXT and RFC 6376 section 3.6.2.2 do, the strings of one
// record are joined into one; separate records stay separate. A name with
// no TXT record (NXDOMAIN, or no data), or one that cannot be a DNS name, is
// a not-found *net.DNSError as from the system resolver, which the capture
// seals as absent; any other answer it cannot trust is an error that moves
// the capture on.
func dohLookupTXT(endpoint, name string) ([]string, bool, error) {
	notFound := &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	q, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil || !validDNSName(q.String()) {
		return nil, false, notFound
	}
	question := dnsmessage.Question{Name: q, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{RecursionDesired: true})
	b.EnableCompression()
	var opt dnsmessage.ResourceHeader
	if err := opt.SetEDNS0(4096, dnsmessage.RCodeSuccess, true); err != nil {
		return nil, false, err
	}
	if err := errors.Join(b.StartQuestions(), b.Question(question), b.StartAdditionals(), b.OPTResource(opt, dnsmessage.OPTResource{})); err != nil {
		return nil, false, err
	}
	query, err := b.Finish()
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(query))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := dohClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";"); !strings.EqualFold(strings.TrimSpace(ct), "application/dns-message") {
		return nil, false, fmt.Errorf("answered %q, not a DNS message", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDoHResponse+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > maxDoHResponse {
		return nil, false, errors.New("answer larger than a DNS message")
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(body); err != nil {
		return nil, false, fmt.Errorf("malformed DNS answer: %w", err)
	}
	if !msg.Response || msg.ID != 0 || len(msg.Questions) != 1 || !sameQuestion(msg.Questions[0], question) {
		return nil, false, errors.New("the answer is not for the question asked")
	}
	if msg.Truncated {
		// Part of an answer proves nothing absent.
		return nil, false, errors.New("the answer is truncated")
	}
	if !msg.RecursionAvailable {
		// A referral or a non-recursive answer has not looked the name up.
		return nil, false, errors.New("the endpoint does not resolve recursively")
	}
	rcode, opts := msg.RCode, 0
	for _, rr := range msg.Additionals {
		if rr.Header.Type != dnsmessage.TypeOPT {
			continue
		}
		// The OPT record carries the RCODE's upper bits (RFC 6891), read
		// only from the one version-0 OPT a well-formed answer has.
		if opts++; opts > 1 || rr.Header.TTL>>16&0xff != 0 {
			return nil, false, errors.New("the answer's EDNS record is malformed")
		}
		rcode = rr.Header.ExtendedRCode(msg.RCode)
	}
	validated := msg.AuthenticData
	switch rcode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return nil, validated, notFound
	default:
		return nil, false, fmt.Errorf("the resolver answered %s", rcode)
	}
	cur := question.Name
	for hops := 0; hops < 8; hops++ {
		var txt []string
		next := ""
		for _, rr := range msg.Answers {
			if !strings.EqualFold(rr.Header.Name.String(), cur.String()) || rr.Header.Class != dnsmessage.ClassINET {
				continue
			}
			switch body := rr.Body.(type) {
			case *dnsmessage.TXTResource:
				txt = append(txt, strings.Join(body.TXT, ""))
			case *dnsmessage.CNAMEResource:
				next = body.CNAME.String()
			}
		}
		if len(txt) > 0 {
			return txt, validated, nil
		}
		if next == "" {
			return nil, validated, notFound
		}
		if cur, err = dnsmessage.NewName(next); err != nil {
			return nil, false, err
		}
	}
	return nil, false, errors.New("too many CNAMEs in the answer")
}

func sameQuestion(a, b dnsmessage.Question) bool {
	return strings.EqualFold(a.Name.String(), b.Name.String()) && a.Type == b.Type && a.Class == b.Class
}

// validDNSName is the system resolver's rule for a name it will look up
// (labels of 1 to 63 octets, at most 254 in all with the root): dnsmessage
// accepts some names a resolver never would.
func validDNSName(fqdn string) bool {
	if fqdn == "." || len(fqdn) > 254 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(fqdn, "."), ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
	}
	return true
}
