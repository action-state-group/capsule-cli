package deal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/capsule-cli/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/fxamacker/cbor/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/veraison/go-cose"
)

type mapArtifactStore map[string]artifact.Record

func (s mapArtifactStore) Get(_ context.Context, id string) (artifact.Record, error) {
	record, ok := s[id]
	if !ok {
		return artifact.Record{}, artifact.ErrNotFound
	}
	return record, nil
}

func bundleRecord(t *testing.T, key ed25519.PrivateKey, chain *emit.Chain, references []emit.Reference) artifact.Record {
	t.Helper()
	request, err := cli.ParseRequest(requestFixture(t))
	require.NoError(t, err)
	request.Capsule.Chain = chain
	request.Capsule.References = references
	record, err := cli.Seal(request, key)
	require.NoError(t, err)
	return record
}

const testWitness = "https://witness.example"

// mintReceipt is an RFC 9162 COSE receipt from a one-entry log over the
// signed checkpoint's entry hash, signed by key (as the witness protocol's
// own tests mint them).
func mintReceipt(t *testing.T, entry []byte, key ed25519.PrivateKey) []byte {
	t.Helper()
	proof, err := cbor.Marshal([]any{int64(1), int64(0), [][]byte{}})
	require.NoError(t, err)
	leaf := sha256.Sum256(append([]byte{0}, entry...))
	message := cose.NewSign1Message()
	message.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	message.Headers.Protected[int64(395)] = int64(1)
	message.Headers.Unprotected[int64(396)] = map[any]any{int64(-1): []any{proof}}
	message.Payload = leaf[:]
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, key)
	require.NoError(t, err)
	require.NoError(t, message.Sign(rand.Reader, nil, signer))
	message.Payload = nil
	encoded, err := message.MarshalCBOR()
	require.NoError(t, err)
	return encoded
}

// witnessed adds one receipt from testWitness, signed by key, over the
// bundle's signed checkpoint.
func witnessed(t *testing.T, key ed25519.PrivateKey) func(map[string]interface{}) {
	return func(b map[string]interface{}) {
		cp := b["checkpoint"].(map[string]interface{})
		statement, err := base64.RawURLEncoding.DecodeString(cp["cose"].(string))
		require.NoError(t, err)
		record, err := checkpoint.ParseRecord(statement)
		require.NoError(t, err)
		entry, err := record.EntryHash()
		require.NoError(t, err)
		cp["witnesses"] = []interface{}{map[string]interface{}{
			"ts_url":      testWitness,
			"entry_hash":  hex.EncodeToString(entry),
			"receipt_b64": base64.StdEncoding.EncodeToString(mintReceipt(t, entry, key)),
			"leaf_index":  "0",
			"tree_size":   json.Number("1"),
		}}
	}
}

func writeDirectory(t *testing.T, rows ...map[string]interface{}) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "witnesses.json")
	data, err := json.Marshal(map[string]interface{}{"directory_version": "1", "witnesses": rows})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func rawKeyRow(endpoint string, public ed25519.PublicKey) map[string]interface{} {
	return map[string]interface{}{"name": "test witness", "endpoint": endpoint, "since": "2026-01-01", "key_ids": []string{hex.EncodeToString(public)}}
}

func verifyWithDirectory(t *testing.T, path, directory string) (map[string]interface{}, error) {
	t.Helper()
	args := []string{"verify", "--bundle", path}
	if directory != "" {
		args = append(args, "--witness-directory", directory)
	}
	out, err := invoke(t, "", args...)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result, err
}

func bundleWitnesses(result map[string]interface{}) (string, []interface{}) {
	w := result["witnesses"].(map[string]interface{})
	return w["status"].(string), w["findings"].([]interface{})
}

func profileFixture(t *testing.T) (cli.Profile, ed25519.PrivateKey) {
	t.Helper()
	public, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	p := cli.Profile{Name: "test", Type: "mysql", LogID: "test-log", Namespace: "capsule", Operator: "test-operator"}
	p.Connection.Host = "127.0.0.1"
	p.Connection.Port = 3306
	p.Connection.Database = "capsule_cli_test"
	p.Connection.TLS = "false"
	p.Credentials.Username = "root"
	p.Signing.Value = hex.EncodeToString(private.Seed())
	p.TrustedKeys = []string{hex.EncodeToString(public)}
	p.Checkpoint.Signing = p.Signing
	p.Checkpoint.TrustedKeys = p.TrustedKeys
	return p, private
}

func requestFixture(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"test-action","ActionType":"fyi","Operator":"test-operator","Developer":"test-developer","Timestamp":"2026-09-08T00:00:00Z"},"payload":{"a": 1},"agent_output":{"ok":true}}`)
}

func invoke(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	c := testCommand()
	var out bytes.Buffer
	c.SetIn(strings.NewReader(input))
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	e := c.ExecuteContext(t.Context())
	return out.String(), e
}

// testCommand is the capsulectl tree as a user reaches it with the deal
// plugin installed, in one process: the core commands, and `deal ...` as the
// core dispatches it.
func testCommand() *cobra.Command {
	return cli.NewCommandWith(DealGroup())
}

func writeBundle(t *testing.T, value interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bundle.json")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// signCountersignEntry sets entry.Signature to key's signature over the
// entry's countersign/v1 signing input.
func signCountersignEntry(t *testing.T, key ed25519.PrivateKey, entry *cli.CountersignatureEntry) {
	t.Helper()
	statement, err := cli.JSONValue(entry.Statement)
	require.NoError(t, err)
	signer, err := cli.JSONValue(entry.Signer)
	require.NoError(t, err)
	message, err := cli.CountersignSigningInput(entry.Over, signer, statement, entry.Type)
	require.NoError(t, err)
	entry.Signature = hex.EncodeToString(ed25519.Sign(key, message))
}

// countersignerEntry signs a bundle digest and statement with a fresh
// countersigner key and returns the wire entry plus that key's hex public key.
func countersignerEntry(t *testing.T, digest string, checks []cli.CountersignCheck) (cli.CountersignatureEntry, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	entry := cli.CountersignatureEntry{
		Type:      cli.CountersignAPI,
		Signer:    cli.CountersignSigner{ID: "countersign.example", KeyID: hex.EncodeToString(public)},
		Over:      digest,
		Statement: cli.CountersignStatement{Checks: checks, RecomputedAt: "2026-09-16T00:00:00Z", Scope: cli.CountersignScope{LedgerID: "test-log", ClosureDepth: json.Number("2")}},
	}
	signCountersignEntry(t, private, &entry)
	return entry, hex.EncodeToString(public)
}

func mustDigest(t *testing.T, bundle map[string]interface{}) string {
	t.Helper()
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)
	return digest
}

func writeJSONFile(t *testing.T, name string, value any) string {
	t.Helper()
	b, e := json.Marshal(value)
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return path
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// headlessChrome drives one headless Chrome over the DevTools protocol on a
// pipe (--remote-debugging-pipe: commands on fd 3, replies on fd 4, each
// message JSON ended by a NUL byte), so the presentation goldens can set a
// viewport and print media exactly, with no browser library dependency.
type headlessChrome struct {
	cmd    *exec.Cmd
	in     *os.File
	mu     sync.Mutex
	nextID int
	reply  map[int]chan cdpMessage
	events chan cdpMessage
	done   chan struct{}
	// requests holds, per page session, every URL the page, its iframes or
	// its workers asked for, WebSockets included. Under "" are the requests
	// of service and shared workers, which belong to no one page.
	requests map[string][]string
	// page maps each auto-attached child session (an iframe's or a
	// worker's) to the page session it belongs to.
	page map[string]string
	// childErrors are failures to watch a child target, which would let
	// its requests go unseen; watching counts the child targets still
	// being set up.
	childErrors []string
	watching    int
}

type cdpMessage struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func startHeadlessChrome(t *testing.T, binary string) *headlessChrome {
	t.Helper()
	toChrome, commands, err := os.Pipe()
	require.NoError(t, err)
	replies, fromChrome, err := os.Pipe()
	require.NoError(t, err)
	// The profile directory is the test's own, removed only once every
	// Chrome process has exited (a child process can still be writing to it
	// after the browser process is gone).
	profile, err := os.MkdirTemp("", "capsulectl-chrome-")
	require.NoError(t, err)
	// No page may reach the network: every host, IP literals and loopback
	// included, resolves to nothing, which also stops connections no request
	// event reports, such as a preconnect
	// (TestPresentationChromeSeesExternalRequests checks both). Each page's
	// requests are checked as well (see open).
	cmd := exec.Command(binary, "--headless=new", "--remote-debugging-pipe", "--no-sandbox", "--lang=en-US",
		"--disable-gpu", "--no-first-run", "--no-default-browser-check", "--hide-scrollbars",
		"--host-resolver-rules=MAP * ~NOTFOUND", "--disable-background-networking",
		"--disable-component-update", "--disable-sync",
		// A fresh profile would otherwise make Chrome create its Safe
		// Storage key in the OS keychain, which on macOS asks the person
		// at the screen (a "Keychain Not Found" dialog) from a test.
		"--use-mock-keychain", "--password-store=basic",
		"--user-data-dir="+profile, "about:blank")
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	// The deal page shows local times: the goldens are taken in UTC, in
	// en-US, wherever the test runs.
	cmd.Env = append(os.Environ(), "TZ=UTC", "LANG=en_US.UTF-8")
	chromeProcessGroup(cmd)
	require.NoError(t, cmd.Start())
	require.NoError(t, toChrome.Close())
	require.NoError(t, fromChrome.Close())
	c := &headlessChrome{cmd: cmd, in: commands, reply: map[int]chan cdpMessage{},
		events: make(chan cdpMessage, 1024), done: make(chan struct{}), requests: map[string][]string{},
		page: map[string]string{}}
	go c.read(replies)
	t.Cleanup(func() { stopHeadlessChrome(t, cmd, commands, profile) })
	// Service and shared workers are targets of the browser, not of a page.
	require.NoError(t, c.call("", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []map[string]any{{"type": "service_worker"}, {"type": "shared_worker"}}}, nil))
	return c
}

// stopHeadlessChrome closes the DevTools pipe, on which Chrome exits, and
// waits for it; past a deadline it kills it. Then it kills whatever is left
// of Chrome's process group (its renderer and helper processes) and removes
// the profile directory, retrying while a process that was just killed
// releases its files. A directory that still will not go is logged, not a
// failure: it is a leftover in the temporary directory, not a test result.
func stopHeadlessChrome(t *testing.T, cmd *exec.Cmd, commands *os.File, profile string) {
	_ = commands.Close()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		killChromeProcessGroup(cmd)
		<-exited
	}
	killChromeProcessGroup(cmd)
	var err error
	for range 20 {
		if err = os.RemoveAll(profile); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("the Chrome profile directory %s was left behind: %v", profile, err)
}

func (c *headlessChrome) read(replies *os.File) {
	defer close(c.done)
	r := bufio.NewReader(replies)
	for {
		raw, err := r.ReadBytes(0)
		if err != nil {
			return
		}
		var m cdpMessage
		if json.Unmarshal(raw[:len(raw)-1], &m) != nil {
			continue
		}
		if m.ID == 0 {
			switch m.Method {
			case "Network.requestWillBeSent":
				var sent struct {
					Request struct {
						URL string `json:"url"`
					} `json:"request"`
				}
				if json.Unmarshal(m.Params, &sent) == nil {
					c.record(m.SessionID, sent.Request.URL)
				}
			case "Network.webSocketCreated":
				var created struct {
					URL string `json:"url"`
				}
				if json.Unmarshal(m.Params, &created) == nil {
					c.record(m.SessionID, created.URL)
				}
			case "Target.attachedToTarget":
				var attached struct {
					SessionID  string `json:"sessionId"`
					TargetInfo struct {
						Type string `json:"type"`
					} `json:"targetInfo"`
				}
				// At the browser level only service and shared workers are
				// watched: the pages that load attaches to are reported
				// there too.
				if json.Unmarshal(m.Params, &attached) == nil && (m.SessionID != "" ||
					attached.TargetInfo.Type == "service_worker" || attached.TargetInfo.Type == "shared_worker") {
					c.mu.Lock()
					c.page[attached.SessionID] = c.pageOf(m.SessionID)
					c.watching++
					c.mu.Unlock()
					// The reader cannot wait for replies itself.
					go c.watchChild(attached.SessionID)
				}
			}
			select {
			case c.events <- m:
			default:
			}
			continue
		}
		c.mu.Lock()
		ch := c.reply[m.ID]
		delete(c.reply, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// pageOf is the page session a session belongs to: itself for a page, ""
// for a browser-level worker. The caller holds mu.
func (c *headlessChrome) pageOf(session string) string {
	for {
		parent, ok := c.page[session]
		if !ok {
			return session
		}
		session = parent
	}
}

// record files url under the page session that asked for it.
func (c *headlessChrome) record(session, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	page := c.pageOf(session)
	c.requests[page] = append(c.requests[page], url)
}

// watchChild records an auto-attached iframe's or worker's requests, and
// those of its own children, then lets it run: it was paused on start, so
// none of its requests is missed. A failure is kept for the page's check,
// unless the target is already gone (nothing of it is left to watch).
func (c *headlessChrome) watchChild(session string) {
	defer func() {
		c.mu.Lock()
		c.watching--
		c.mu.Unlock()
	}()
	for _, step := range []struct {
		method string
		params map[string]any
	}{
		{"Network.enable", map[string]any{}},
		{"Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}},
		{"Runtime.runIfWaitingForDebugger", map[string]any{}},
	} {
		if err := c.call(session, step.method, step.params, nil); err != nil && !targetGone(err) {
			c.mu.Lock()
			c.childErrors = append(c.childErrors, err.Error())
			c.mu.Unlock()
		}
	}
}

// call sends one command (to a page session when session is set) and waits
// for its reply.
func (c *headlessChrome) call(session, method string, params any, result any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan cdpMessage, 1)
	c.reply[id] = ch
	c.mu.Unlock()
	message := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		message["sessionId"] = session
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := c.in.Write(append(raw, 0)); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-c.done:
		return errors.New("chrome exited")
	case <-time.After(60 * time.Second):
		return fmt.Errorf("%s: no reply in 60s", method)
	}
}

// pageView is one viewport a page is opened at.
type pageView struct {
	name          string
	width, height int
	mobile        bool
	media         string // "" (screen) or "print"
	// scriptsOff loads the page with JavaScript off, as a script-blocking
	// viewer or a host's inline preview shows it.
	scriptsOff bool
}

var presentationViews = []pageView{
	{name: "390", width: 390, height: 844, mobile: true},
	{name: "1280", width: 1280, height: 900},
	{name: "print", width: 794, height: 1123, media: "print"},
}

// openedPage is a page in one view, after its scripts rendered it.
type openedPage struct {
	c       *headlessChrome
	session string
	target  string
}

// errorHook records every console.error, uncaught error and unhandled
// rejection before the page's own scripts run.
const errorHook = `(function(){window.__renderErrors=[];var e=console.error;console.error=function(){window.__renderErrors.push("console.error: "+Array.prototype.join.call(arguments," "));return e.apply(console,arguments);};window.addEventListener("error",function(ev){window.__renderErrors.push("uncaught: "+ev.message);});document.addEventListener("securitypolicyviolation",function(ev){window.__renderErrors.push("CSP violation: "+ev.violatedDirective+" "+(ev.blockedURI||"inline"));});window.addEventListener("unhandledrejection",function(ev){window.__renderErrors.push("unhandled rejection: "+(ev.reason&&ev.reason.message||ev.reason));});})();`

// rendered waits until the viewer has drawn its verification page (and a
// deal page its deal section), then lets pending work settle.
const rendered = `(async () => {
  const done = () => document.querySelector('[data-page="verification"]') !== null &&
    (document.getElementById("deal") === null || document.getElementById("deal").childElementCount > 0);
  const deadline = Date.now() + 30000;
  while (!done() && Date.now() < deadline) await new Promise(r => setTimeout(r, 50));
  await new Promise(r => setTimeout(r, 300));
  return done();
})()`

func (c *headlessChrome) open(t *testing.T, url string, view pageView) openedPage {
	t.Helper()
	p := c.load(t, url, view)
	var ok bool
	p.eval(t, rendered, &ok)
	require.True(t, ok, "the page did not finish rendering its verification page in 30s (%s, %s)", url, view.name)
	var errs []string
	p.eval(t, `window.__renderErrors || ["the error hook did not run"]`, &errs)
	require.Empty(t, errs, "console errors, uncaught errors or unhandled rejections (%s)", view.name)
	// Waits for every iframe and worker to be set up, so none is unwatched.
	require.Empty(t, c.watchErrors(), "an iframe or worker could not be watched (%s)", view.name)
	require.Empty(t, c.externalRequests(p.session), "the page asked for something outside itself (%s)", view.name)
	return p
}

// load opens url in a new page in view and waits for its load event.
func (c *headlessChrome) load(t *testing.T, url string, view pageView) openedPage {
	t.Helper()
	var target struct {
		TargetID string `json:"targetId"`
	}
	require.NoError(t, c.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target))
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, c.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached))
	s := attached.SessionID
	p := openedPage{c: c, session: s, target: target.TargetID}
	t.Cleanup(func() { _ = c.call("", "Target.closeTarget", map[string]any{"targetId": target.TargetID}, nil) })
	require.NoError(t, c.call(s, "Page.enable", map[string]any{}, nil))
	require.NoError(t, c.call(s, "Network.enable", map[string]any{}, nil))
	// Iframes in another process and dedicated workers are targets of their
	// own; their requests are the page's.
	require.NoError(t, c.call(s, "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}, nil))
	require.NoError(t, c.call(s, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": errorHook}, nil))
	require.NoError(t, c.call(s, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": view.width, "height": view.height, "deviceScaleFactor": 1, "mobile": view.mobile}, nil))
	require.NoError(t, c.call(s, "Emulation.setEmulatedMedia", map[string]any{"media": view.media}, nil))
	if view.scriptsOff {
		require.NoError(t, c.call(s, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil))
	}
	require.NoError(t, c.call(s, "Page.navigate", map[string]any{"url": url}, nil))
	c.waitFor(t, s, "Page.loadEventFired")
	return p
}

// targetGone reports whether a command failed because its target had
// already closed or navigated away.
func targetGone(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "ession with given id not found") ||
		strings.Contains(msg, "Inspected target navigated or closed") ||
		strings.Contains(msg, "Target closed")
}

// watchErrors is every failure to watch a child target, once every child
// attached so far is set up (each command gives up within call's 60s).
func (c *headlessChrome) watchErrors() []string {
	for {
		c.mu.Lock()
		if c.watching == 0 {
			defer c.mu.Unlock()
			return slices.Clone(c.childErrors)
		}
		c.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
}

// externalRequests is every URL a page session (or a service or shared
// worker) asked for that is not the page itself or data it carries.
func (c *headlessChrome) externalRequests(session string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, u := range append(slices.Clone(c.requests[session]), c.requests[""]...) {
		// chrome-extension: is the browser's own component extensions (such
		// as text-to-speech) loading their files, not a page's request.
		if !strings.HasPrefix(u, "file:") && !strings.HasPrefix(u, "data:") &&
			!strings.HasPrefix(u, "blob:") && !strings.HasPrefix(u, "about:") &&
			!strings.HasPrefix(u, "chrome-extension:") {
			out = append(out, u)
		}
	}
	return out
}

// version is the browser's product string, e.g. "Chrome/155.0.8059.39".
func (c *headlessChrome) version(t *testing.T) string {
	t.Helper()
	var v struct {
		Product string `json:"product"`
	}
	require.NoError(t, c.call("", "Browser.getVersion", map[string]any{}, &v))
	return v.Product
}

func (c *headlessChrome) waitFor(t *testing.T, session, method string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case m := <-c.events:
			if m.Method == method && m.SessionID == session {
				return
			}
		case <-deadline:
			t.Fatalf("no %s in 60s", method)
		}
	}
}

// eval runs expression in the page (awaiting a promise) and decodes its
// JSON-able value into out.
func (p openedPage) eval(t *testing.T, expression string, out any) {
	t.Helper()
	var reply struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	require.NoError(t, p.c.call(p.session, "Runtime.evaluate", map[string]any{
		"expression": expression, "awaitPromise": true, "returnByValue": true}, &reply))
	if reply.ExceptionDetails != nil {
		detail := reply.ExceptionDetails.Text
		if reply.ExceptionDetails.Exception != nil {
			detail = reply.ExceptionDetails.Exception.Description
		}
		t.Fatalf("page script threw: %s", detail)
	}
	require.NoError(t, json.Unmarshal(reply.Result.Value, out))
}

// capture writes the view as a full-page PNG, or a PDF for print media.
func (p openedPage) capture(t *testing.T, view pageView, path string) {
	t.Helper()
	var shot struct {
		Data string `json:"data"`
	}
	if view.media == "print" {
		require.NoError(t, p.c.call(p.session, "Page.printToPDF", map[string]any{"printBackground": true}, &shot))
	} else {
		require.NoError(t, p.c.call(p.session, "Page.captureScreenshot", map[string]any{
			"format": "png", "captureBeyondViewport": true}, &shot))
	}
	raw, err := base64.StdEncoding.DecodeString(shot.Data)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}

// raceStride keeps one case in raceStride under the race detector.
const raceStride = 8

func raceSampling() bool {
	return raceDetector && os.Getenv("CAPSULE_TEST_RACE_FULL") == ""
}

// raceRounds is n, or a tenth of it (at least one) under the race detector.
func raceRounds(n int) int {
	if raceSampling() {
		return max(1, n/10)
	}
	return n
}

// raceSample skips case i of a list unless it is in the race detector's
// sample. Case 0 always runs.
func raceSample(t *testing.T, i int) {
	t.Helper()
	if raceSampling() && i%raceStride != 0 {
		t.Skip("sampled under the race detector; every case runs without it")
	}
}

// raceSampleKey is raceSample for a case named by key (map iteration has no
// stable index): a fixed hash of the name decides.
func raceSampleKey(t *testing.T, key string) {
	t.Helper()
	h := fnv.New32a()
	h.Write([]byte(key))
	raceSample(t, int(h.Sum32()%raceStride))
}

// raceSkip skips a whole end-to-end test under the race detector, for one
// that runs one command at a time and whose work no other test shares. It
// runs in full without it.
func raceSkip(t *testing.T) {
	t.Helper()
	if raceSampling() {
		t.Skip("skipped under the race detector; it runs in full without it")
	}
}

func verifyBundleOutput(t *testing.T, path string) (map[string]interface{}, error) {
	t.Helper()
	out, err := invoke(t, "", "verify", "--bundle", path)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result, err
}

func status(result map[string]interface{}, claim string) string {
	return result[claim].(map[string]interface{})["status"].(string)
}
