package deal

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This is cli's test of the same recorder, which this package
// copies (corehelpers_test.go): CI runs Chrome for this package's goldens.

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
	// A connection accepted just before the read could be counted after it.
	time.Sleep(200 * time.Millisecond)
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

// watchErrors waits for a child still being set up, and returns the
// failure that setup ends in.
func TestWatchErrorsWaitsForAChildBeingSetUp(t *testing.T) {
	c := &headlessChrome{watching: 1}
	go func() {
		time.Sleep(200 * time.Millisecond)
		c.mu.Lock()
		c.childErrors = append(c.childErrors, "Network.enable: failed")
		c.watching--
		c.mu.Unlock()
	}()
	// The failure is appended only as the setup ends, so seeing it proves
	// the wait.
	assert.Equal(t, []string{"Network.enable: failed"}, c.watchErrors())
}
