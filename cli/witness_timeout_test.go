package cli

import (
	"context"
	"crypto/ed25519"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A short request deadline keeps the stall tests fast; the CLI's own is 30 s
// (witnessBinding).
const stallTimeout = 200 * time.Millisecond

// stallingWitness is a TLS witness whose handler blocks until the test ends;
// before blocking it runs first, which may write part of an answer.
func stallingWitness(t *testing.T, first func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first != nil {
			first(w)
		}
		<-release
	}))
	// Cleanups run last-in first-out: release the handlers, then close.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

// holdingListener accepts TCP connections and never says anything on them,
// as a host that holds a connection while it waits on a network-consent
// answer does.
func holdingListener(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, e)
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	})
	return l.Addr().String()
}

// deliverOnceVia runs one delivery of service-test's checkpoint 1 to
// endpoint with client and returns the stored row.
func deliverOnceVia(t *testing.T, store cll.WitnessStateStore, public ed25519.PublicKey, endpoint string, client *http.Client) cll.WitnessState {
	t.Helper()
	c, e := witness.NewClient(endpoint, client, 0)
	require.NoError(t, e)
	verifier, e := witness.NewReceiptVerifier(public)
	require.NoError(t, e)
	runner, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: store, id: "service-test", size: 1}, map[string]witness.Submitter{"service-test": safeSubmitter{c}}, map[string]witness.Verifier{"service-test": verifier})
	require.NoError(t, e)
	_, e = runner.RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	return saved
}

// A witness that was reached and then did not answer in time (no headers,
// or headers and a stalled body) is a slow witness: it is retried and never
// worded as network consent (capsule-cli#643).
func TestWitnessTimeoutAfterConnectingIsSlowNotConsent(t *testing.T) {
	cases := map[string]func(http.ResponseWriter){
		"no answer": nil,
		"stalled body": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"receipt_b64":`))
			w.(http.Flusher).Flush()
		},
	}
	for name, first := range cases {
		t.Run(name, func(t *testing.T) {
			_, store, public, _ := witnessReceiptFixture(t)
			defer func() { require.NoError(t, store.Close()) }()
			server := stallingWitness(t, first)
			saved := deliverOnceVia(t, store, public, server.URL, &http.Client{Transport: bearerTransport{base: server.Client().Transport}, Timeout: stallTimeout})
			assert.False(t, saved.Permanent, "a slow witness is retried")
			assert.Contains(t, saved.LastError, WitnessTimedOut)
			assert.NotContains(t, saved.LastError, WitnessNotReached)
			reason, text := WitnessPendingReason(saved, testWitness)
			assert.Equal(t, "witness_timeout", reason)
			assert.Equal(t, "pending: the witness was reached but its answer did not come in time, so the witness or the network to it is slow; it is tried again on a later capsulectl run, after a wait that grows up to an hour.", text)
			assert.NotContains(t, text, "consent")
		})
	}
}

// Every way a request can fail without a connection to the witness is what
// a host withholding network consent produces, so it keeps the consent
// text, a timeout before connecting included.
func TestWitnessNotReachedStaysConsent(t *testing.T) {
	tlsWitness := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsWitness.Close()
	tlsTransport := tlsWitness.Client().Transport.(*http.Transport)
	held := holdingListener(t)
	proxy := holdingListener(t)
	proxyURL, e := url.Parse("http://" + proxy)
	require.NoError(t, e)
	viaProxy := tlsTransport.Clone()
	viaProxy.Proxy = http.ProxyURL(proxyURL)

	cases := map[string]struct {
		endpoint string
		client   *http.Client
	}{
		"connection refused":  {"https://127.0.0.1:1", &http.Client{Timeout: stallTimeout}},
		"name not resolved":   {"https://witness.invalid", &http.Client{Timeout: stallTimeout}},
		"TLS handshake held":  {"https://" + held, &http.Client{Transport: tlsTransport.Clone(), Timeout: stallTimeout}},
		"proxy holds CONNECT": {tlsWitness.URL, &http.Client{Transport: viaProxy, Timeout: stallTimeout}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, store, public, _ := witnessReceiptFixture(t)
			defer func() { require.NoError(t, store.Close()) }()
			saved := deliverOnceVia(t, store, public, c.endpoint, c.client)
			assert.False(t, saved.Permanent)
			reason, text := WitnessPendingReason(saved, testWitness)
			assert.Equal(t, "network_consent_needed", reason)
			assert.Equal(t, WitnessConsentText(testWitness), text)
		})
	}
}

// The classes by error alone: what counts is whether the request had a
// connection to the witness, and an answered status keeps its own path.
func TestClassifySubmitErrorClasses(t *testing.T) {
	dnsTimeout := &url.Error{Op: "Post", URL: testWitness, Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "i/o timeout", Name: "witness.example", IsTimeout: true}}}
	nxdomain := &url.Error{Op: "Post", URL: testWitness, Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "witness.example", IsNotFound: true}}}
	cases := []struct {
		name      string
		err       error
		connected bool
		body      string
		status    int
	}{
		{"deadline after connecting", context.DeadlineExceeded, true, WitnessTimedOut, 503},
		{"client timeout after connecting", &url.Error{Op: "Post", URL: testWitness, Err: &net.OpError{Op: "read", Err: context.DeadlineExceeded}}, true, WitnessTimedOut, 503},
		{"deadline before connecting", context.DeadlineExceeded, false, WitnessNotReached, 503},
		{"DNS timeout", dnsTimeout, false, WitnessNotReached, 503},
		{"name not found", nxdomain, false, WitnessNotReached, 503},
		{"cancelled after connecting", context.Canceled, true, WitnessNotReached, 503},
		{"proxy authorization", &witness.HTTPError{StatusCode: http.StatusProxyAuthRequired}, false, WitnessNotReached, 503},
		{"answered 500", &witness.HTTPError{StatusCode: 500, Body: "x"}, true, WitnessFailureBody(500), 503},
		{"answered 403", &witness.HTTPError{StatusCode: 403, Body: "x"}, true, WitnessFailureBody(403), 503},
		{"answered 409", &witness.HTTPError{StatusCode: 409, Body: "x"}, true, WitnessFailureBody(409), 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got *witness.HTTPError
			require.ErrorAs(t, classifySubmitError(c.err, c.connected), &got)
			assert.Equal(t, c.status, got.StatusCode)
			assert.Equal(t, c.body, got.Body)
		})
	}
	assert.NoError(t, classifySubmitError(nil, true))
}

// A caller's own deadline ends the delivery without recording anything (the
// library returns ctx.Err() before it commits), so no pending text comes
// from it: the row stays as it was.
func TestWitnessCallerDeadlineLeavesRow(t *testing.T) {
	_, store, public, _ := witnessReceiptFixture(t)
	defer func() { require.NoError(t, store.Close()) }()
	server := stallingWitness(t, nil)
	before, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	c, e := witness.NewClient(server.URL, &http.Client{Transport: bearerTransport{base: server.Client().Transport}}, 0)
	require.NoError(t, e)
	verifier, e := witness.NewReceiptVerifier(public)
	require.NoError(t, e)
	runner, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: store, id: "service-test", size: 1}, map[string]witness.Submitter{"service-test": safeSubmitter{c}}, map[string]witness.Verifier{"service-test": verifier})
	require.NoError(t, e)
	ctx, cancel := context.WithTimeout(t.Context(), stallTimeout)
	defer cancel()
	_, e = runner.RunOnce(ctx, time.Now().UTC(), 1)
	require.ErrorIs(t, e, context.DeadlineExceeded)
	after, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	assert.Equal(t, before, after)
}

// A SCRAPI witness that accepted the registration (202) and then let the
// poll run past the deadline was reached: it reads as slow, not consent.
func TestSCRAPIAcceptedThenDeadlineIsSlow(t *testing.T) {
	p, _ := profileFixture(t)
	public, _, e := ed25519.GenerateKey(nil)
	require.NoError(t, e)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/operations/1")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	p.Checkpoint.Endpoint = "scrapi+" + server.URL
	_, store, _, _ := witnessReceiptFixture(t)
	defer func() { require.NoError(t, store.Close()) }()
	client, e := witness.NewSCRAPIClient(witnessHTTPS(p.Checkpoint.Endpoint), &http.Client{Transport: bearerTransport{base: server.Client().Transport}, Timeout: stallTimeout}, 0)
	require.NoError(t, e)
	verifier, e := witness.NewReceiptVerifier(public)
	require.NoError(t, e)
	runner, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: store, id: "service-test", size: 1}, map[string]witness.Submitter{"service-test": scrapiSubmitter{client}}, map[string]witness.Verifier{"service-test": verifier})
	require.NoError(t, e)
	_, e = runner.RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	reason, _ := WitnessPendingReason(saved, testWitness)
	assert.Equal(t, "witness_timeout", reason)
}

// doctor's probe: a witness reached but slower than the probe's deadline is
// reported as timed out, with no consent text.
func TestDoctorWitnessProbeTimeoutIsNotConsent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	saved := witnessProbeTimeout
	witnessProbeTimeout = stallTimeout
	t.Cleanup(func() { witnessProbeTimeout = saved })
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	// Cleanups run last-in first-out: release the handler, then close, so a
	// failed assertion cannot leave Close waiting on it.
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	p, _ := profileFixture(t)
	p.Checkpoint.Endpoint = server.URL
	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	require.NoError(t, SaveProfile(p, false))

	report := doctorReport(t, "--profile", p.Name, "--check-witness")
	w, _ := report["witness"].(map[string]any)
	assert.Equal(t, true, w["timed_out"])
	assert.Equal(t, false, w["reachable"])
	assert.Equal(t, false, w["ok"])
	assert.Equal(t, WitnessConsentReached(p.Checkpoint.Endpoint), w["consent"], "reached: the grant statement, no consent-missing claim")
	assert.NotContains(t, w["consent"], "consent needed")
	assert.Equal(t, "the checkpoint endpoint was reached but did not answer the probe within 200ms: the witness or the network to it is slow", w["issue"])

	// Not reached at all: the consent text, as before. Release the held
	// handler first, or Close waits on it.
	unblock()
	server.Close()
	report = doctorReport(t, "--profile", p.Name, "--check-witness")
	w, _ = report["witness"].(map[string]any)
	assert.NotContains(t, w, "timed_out")
	assert.Equal(t, WitnessConsentText(p.Checkpoint.Endpoint), w["consent"])
}
