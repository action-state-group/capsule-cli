package cli

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// memoryRelay is a NIP-01 relay that keeps what it is sent in memory: it
// answers EVENT with OK, and REQ with the stored events its filter matches
// (kinds, ids, authors and single-letter tag filters) and then EOSE. It
// replaces the latest event per (pubkey, kind, d) in the addressable range.
type memoryRelay struct {
	URL    string
	mu     sync.Mutex
	events []nostrEvent
	// Drop makes the relay take an event and keep nothing: it says OK and
	// stores it nowhere.
	Drop func(nostrEvent) bool
}

func newMemoryRelay(t *testing.T) *memoryRelay {
	t.Helper()
	r := &memoryRelay{}
	srv := httptest.NewServer(websocket.Handler(r.serve))
	t.Cleanup(srv.Close)
	old := nostrDialInsecure
	nostrDialInsecure = true
	t.Cleanup(func() { nostrDialInsecure = old })
	oldPrivate := allowPrivateAddresses
	allowPrivateAddresses = true
	t.Cleanup(func() { allowPrivateAddresses = oldPrivate })
	r.URL = "ws" + strings.TrimPrefix(srv.URL, "http")
	return r
}

func (r *memoryRelay) Events() []nostrEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// Put stores an event as if it had been published.
func (r *memoryRelay) Put(ev nostrEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *memoryRelay) serve(ws *websocket.Conn) {
	for {
		var frame []json.RawMessage
		if websocket.JSON.Receive(ws, &frame) != nil || len(frame) < 2 {
			return
		}
		var kind string
		_ = json.Unmarshal(frame[0], &kind)
		switch kind {
		case "EVENT":
			var ev nostrEvent
			if json.Unmarshal(frame[1], &ev) != nil {
				return
			}
			if err := ev.verify(); err != nil {
				_ = websocket.JSON.Send(ws, []any{"OK", ev.ID, false, "invalid: " + err.Error()})
				continue
			}
			if r.Drop == nil || !r.Drop(ev) {
				r.store(ev)
			}
			_ = websocket.JSON.Send(ws, []any{"OK", ev.ID, true, ""})
		case "REQ":
			var sub string
			var filter map[string]json.RawMessage
			_ = json.Unmarshal(frame[1], &sub)
			if len(frame) > 2 {
				_ = json.Unmarshal(frame[2], &filter)
			}
			for _, ev := range r.Events() {
				if matches(filter, ev) {
					_ = websocket.JSON.Send(ws, []any{"EVENT", sub, ev})
				}
			}
			_ = websocket.JSON.Send(ws, []any{"EOSE", sub})
		case "CLOSE":
			return
		}
	}
}

func (r *memoryRelay) store(ev nostrEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.Kind >= 30000 && ev.Kind < 40000 {
		d := tagValue(ev, "d")
		r.events = slices.DeleteFunc(r.events, func(o nostrEvent) bool {
			return o.Pubkey == ev.Pubkey && o.Kind == ev.Kind && tagValue(o, "d") == d
		})
	}
	for _, o := range r.events {
		if o.ID == ev.ID {
			return
		}
	}
	r.events = append(r.events, ev)
}

func tagValue(ev nostrEvent, name string) string {
	for _, tag := range ev.Tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}

func matches(filter map[string]json.RawMessage, ev nostrEvent) bool {
	for name, raw := range filter {
		var want []any
		if json.Unmarshal(raw, &want) != nil {
			continue
		}
		in := func(v string) bool {
			for _, w := range want {
				if s, ok := w.(string); ok && s == v {
					return true
				}
			}
			return false
		}
		switch {
		case name == "kinds":
			ok := false
			for _, w := range want {
				if f, isNum := w.(float64); isNum && int(f) == ev.Kind {
					ok = true
				}
			}
			if !ok {
				return false
			}
		case name == "ids":
			if !in(ev.ID) {
				return false
			}
		case name == "authors":
			if !in(ev.Pubkey) {
				return false
			}
		case strings.HasPrefix(name, "#") && len(name) == 2:
			ok := false
			for _, tag := range ev.Tags {
				if len(tag) >= 2 && tag[0] == name[1:] && in(tag[1]) {
					ok = true
				}
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

var _ = time.Second
