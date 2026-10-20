package cli

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	// its requests go unseen.
	childErrors []string
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
	cmd := exec.Command(binary, "--headless=new", "--remote-debugging-pipe", "--no-sandbox",
		"--disable-gpu", "--no-first-run", "--no-default-browser-check", "--hide-scrollbars",
		"--host-resolver-rules=MAP * ~NOTFOUND", "--disable-background-networking",
		"--disable-component-update", "--disable-sync",
		"--user-data-dir="+profile, "about:blank")
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
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
				// watched: the pages load attaches itself are reported there
				// too.
				if json.Unmarshal(m.Params, &attached) == nil && (m.SessionID != "" ||
					attached.TargetInfo.Type == "service_worker" || attached.TargetInfo.Type == "shared_worker") {
					c.mu.Lock()
					c.page[attached.SessionID] = c.pageOf(m.SessionID)
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
	for _, step := range []struct {
		method string
		params map[string]any
	}{
		{"Network.enable", map[string]any{}},
		{"Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}},
		{"Runtime.runIfWaitingForDebugger", map[string]any{}},
	} {
		if err := c.call(session, step.method, step.params, nil); err != nil && !strings.Contains(err.Error(), "ession with given id not found") {
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
	require.Empty(t, c.externalRequests(p.session), "the page asked for something outside itself (%s)", view.name)
	require.Empty(t, c.watchErrors(), "an iframe or worker could not be watched (%s)", view.name)
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

// A page that asks for anything outside itself is caught, from the page, a
// WebSocket or a worker, and nothing connects out: no host name resolves,
// and neither does an IP literal, loopback included.
func TestPresentationChromeSeesExternalRequests(t *testing.T) {
	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	// A listener on loopback stands in for an IP-literal host: a preconnect,
	// a WebSocket and a fetch aim at it, and it must see no connection.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var connections atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.Close()
		}
	}()
	ip := "http://" + listener.Addr().String()
	ws := "ws://" + listener.Addr().String() + "/socket"
	worker := `fetch("` + ip + `/from-worker").catch(()=>{})`

	c := startHeadlessChrome(t, binary)
	page := filepath.Join(t.TempDir(), "page.html")
	require.NoError(t, os.WriteFile(page, []byte(`<!doctype html><title>t</title>`+
		`<link rel="preconnect" href="`+ip+`"><link rel="dns-prefetch" href="`+ip+`">`+
		`<img src="https://example.com/pixel.png"><script>`+
		`new WebSocket("`+ws+`");`+
		`new Worker(URL.createObjectURL(new Blob([`+strconv.Quote(worker)+`],{type:"text/javascript"})));`+
		`fetch("`+ip+`/direct").catch(()=>{});`+
		`fetch("https://example.org/x").then(()=>{document.title="reached"},()=>{document.title="blocked"})</script>`), 0o600))
	p := c.load(t, "file://"+page, presentationViews[1])
	var title string
	for i := 0; i < 50 && title != "reached" && title != "blocked"; i++ {
		time.Sleep(100 * time.Millisecond)
		p.eval(t, "document.title", &title)
	}
	assert.Equal(t, "blocked", title, "no host name resolves")
	want := []string{"https://example.com/pixel.png", "https://example.org/x", ip + "/direct", ws, ip + "/from-worker"}
	for i := 0; i < 50 && !isSubset(c.externalRequests(p.session), want); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	assert.Subset(t, c.externalRequests(p.session), want, "the page's, the WebSocket's and the worker's requests are seen")
	// A top-level navigation is not held back the way a file page's
	// subresources on loopback may be (local network access).
	c.load(t, ip+"/navigated", presentationViews[1])
	assert.Zero(t, connections.Load(), "no connection reached an IP literal")
	assert.Empty(t, c.watchErrors(), "every worker was watched")
}

func isSubset(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// watchErrors is every failure to watch a child target so far.
func (c *headlessChrome) watchErrors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.childErrors)
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
