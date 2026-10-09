package cli

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

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
	// requests holds, per page session, every URL the page asked for.
	requests map[string][]string
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
	// No page may reach the network: every host name resolves to nothing,
	// and each page's requests are checked as well (see open).
	cmd := exec.Command(binary, "--headless=new", "--remote-debugging-pipe", "--no-sandbox",
		"--disable-gpu", "--no-first-run", "--no-default-browser-check", "--hide-scrollbars",
		"--host-resolver-rules=MAP * ~NOTFOUND", "--disable-background-networking",
		"--disable-component-update", "--disable-sync",
		"--user-data-dir="+t.TempDir(), "about:blank")
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	require.NoError(t, cmd.Start())
	require.NoError(t, toChrome.Close())
	require.NoError(t, fromChrome.Close())
	c := &headlessChrome{cmd: cmd, in: commands, reply: map[int]chan cdpMessage{},
		events: make(chan cdpMessage, 1024), done: make(chan struct{}), requests: map[string][]string{}}
	go c.read(replies)
	t.Cleanup(func() {
		_ = commands.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return c
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
			if m.Method == "Network.requestWillBeSent" {
				var sent struct {
					Request struct {
						URL string `json:"url"`
					} `json:"request"`
				}
				if json.Unmarshal(m.Params, &sent) == nil {
					c.mu.Lock()
					c.requests[m.SessionID] = append(c.requests[m.SessionID], sent.Request.URL)
					c.mu.Unlock()
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
const errorHook = `(function(){window.__renderErrors=[];var e=console.error;console.error=function(){window.__renderErrors.push("console.error: "+Array.prototype.join.call(arguments," "));return e.apply(console,arguments);};window.addEventListener("error",function(ev){window.__renderErrors.push("uncaught: "+ev.message);});window.addEventListener("unhandledrejection",function(ev){window.__renderErrors.push("unhandled rejection: "+(ev.reason&&ev.reason.message||ev.reason));});})();`

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
	require.NoError(t, c.call(s, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": errorHook}, nil))
	require.NoError(t, c.call(s, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": view.width, "height": view.height, "deviceScaleFactor": 1, "mobile": view.mobile}, nil))
	require.NoError(t, c.call(s, "Emulation.setEmulatedMedia", map[string]any{"media": view.media}, nil))
	require.NoError(t, c.call(s, "Page.navigate", map[string]any{"url": url}, nil))
	c.waitFor(t, s, "Page.loadEventFired")
	var ok bool
	p.eval(t, rendered, &ok)
	require.True(t, ok, "the page did not finish rendering its verification page in 30s (%s, %s)", url, view.name)
	var errs []string
	p.eval(t, `window.__renderErrors || ["the error hook did not run"]`, &errs)
	require.Empty(t, errs, "console errors, uncaught errors or unhandled rejections (%s)", view.name)
	require.Empty(t, c.externalRequests(s), "the page asked for something outside itself (%s)", view.name)
	return p
}

// externalRequests is every URL a page session asked for that is not the
// page itself or data it carries.
func (c *headlessChrome) externalRequests(session string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, u := range c.requests[session] {
		if !strings.HasPrefix(u, "file:") && !strings.HasPrefix(u, "data:") &&
			!strings.HasPrefix(u, "blob:") && !strings.HasPrefix(u, "about:") {
			out = append(out, u)
		}
	}
	return out
}

// version is the browser's product string, e.g. "HeadlessChrome/155.0.8059.39".
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
