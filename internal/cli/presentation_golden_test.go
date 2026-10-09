package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/emitter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The presentation goldens (testdata/presentation/, built by
// scripts/presentation-goldens/build-fixtures.sh): synthetic bundles of six
// kinds and the pages capsulectl emitted for them. Each later version of the
// viewer must reproduce what those pages show a reader -- the same
// verification state, the same views, the same evidence identifiers and the
// same words in the same order -- and none may overflow its viewport at
// 390px, 1280px or in print beyond the views known-overflow.json lists.
// Layout itself is not held.
//
// Every run re-emits each page from its bundle through the path capsulectl
// takes and checks the page gate's decision. With CAPSULECTL_CHROME set to a
// Chrome or Chromium binary, each page is also opened headless and its DOM
// compared with snapshot.json. CAPSULECTL_UPDATE_PRESENTATION_SNAPSHOTS=1
// rewrites the snapshots (only when a change to what a reader sees is meant);
// CAPSULECTL_PRESENTATION_RENDERS=DIR also writes each view as a PNG (PDF
// for print) there.

const presentationDir = "testdata/presentation"

type presentationMeta struct {
	Kind     string `json:"kind"`
	Rerender string `json:"rerender"`
	Card     string `json:"card"`
	Page     string `json:"page"`
	Verify   string `json:"verify"`
}

// presentationSnapshot is what a reader is shown, read from the rendered
// DOM: never layout.
type presentationSnapshot struct {
	Verification struct {
		Verify    []string `json:"verify"`
		Verdict   []string `json:"verdict"`
		Sealed    []string `json:"sealed"`
		Unchecked []string `json:"unchecked"`
	} `json:"verification"`
	Views struct {
		Page    []string `json:"page"`
		Notice  []string `json:"notice"`
		Section []string `json:"section"`
	} `json:"views"`
	EvidenceIDs []string `json:"evidence_ids"`
	// Text is every text node outside script and style, whitespace
	// collapsed, in document order. It is compared as the sequence of its
	// words, so markup that only re-wraps the same words does not count as
	// a change.
	Text []string `json:"text"`
}

const snapshotScript = `(() => {
  const skip = new Set(["SCRIPT", "STYLE", "TEMPLATE", "NOSCRIPT"]);
  const hex = /\b[0-9a-f]{64}\b/g;
  const text = [], ids = new Set();
  const walk = (node) => {
    if (node.nodeType === Node.TEXT_NODE) {
      const s = node.nodeValue.replace(/\s+/g, " ").trim();
      if (s) { text.push(s); for (const m of s.matchAll(hex)) ids.add(m[0]); }
      return;
    }
    if (node.nodeType !== Node.ELEMENT_NODE || skip.has(node.tagName)) return;
    for (const a of node.attributes) for (const m of a.value.matchAll(hex)) ids.add(m[0]);
    for (const child of node.childNodes) walk(child);
  };
  walk(document.body);
  const all = (name) => [...document.querySelectorAll("[" + name + "]")].map((e) => e.getAttribute(name));
  return {
    verification: { verify: all("data-verify"), verdict: all("data-verdict"), sealed: all("data-sealed"), unchecked: all("data-unchecked") },
    views: { page: all("data-page"), notice: all("data-notice"), section: all("data-section") },
    evidence_ids: [...ids].sort(),
    text,
  };
})()`

// overflowScript reports how wide the page is against its viewport, and the
// first elements that stick out past it.
const overflowScript = `(() => {
  const width = document.documentElement.clientWidth;
  const out = [];
  for (const e of document.querySelectorAll("body *")) {
    const r = e.getBoundingClientRect();
    if (r.width > 0 && r.right > width + 1) out.push(e.tagName.toLowerCase() + (e.className ? "." + String(e.className).split(" ")[0] : "") + " right=" + Math.round(r.right));
    if (out.length >= 5) break;
  }
  return { scroll_width: document.documentElement.scrollWidth, width, over: out };
})()`

func TestPresentationGoldens(t *testing.T) {
	entries, err := os.ReadDir(presentationDir)
	require.NoError(t, err)
	chromeBinary := os.Getenv("CAPSULECTL_CHROME")
	var chrome *headlessChrome
	if chromeBinary != "" {
		chrome = startHeadlessChrome(t, chromeBinary)
	}
	update := os.Getenv("CAPSULECTL_UPDATE_PRESENTATION_SNAPSHOTS") == "1"
	var knownOverflow map[string]any
	readJSONFile(t, filepath.Join(presentationDir, "known-overflow.json"), &knownOverflow)
	renders := os.Getenv("CAPSULECTL_PRESENTATION_RENDERS")
	seen := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		seen++
		dir := filepath.Join(presentationDir, entry.Name())
		t.Run(entry.Name(), func(t *testing.T) {
			var meta presentationMeta
			readJSONFile(t, filepath.Join(dir, "meta.json"), &meta)
			raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
			require.NoError(t, err)
			bundle, err := decodeBundleJSON(raw)
			require.NoError(t, err)
			verdict, _ := bundleVerdict(bundle, nil)
			require.Equal(t, meta.Verify, verdict, "verify --bundle's verdict on the fixture")

			page, err := rerenderPresentationPage(t, dir, meta, bundle)
			if meta.Page == "refused" {
				require.Error(t, err, "the page gate must refuse this fixture's page")
				assert.Contains(t, err.Error(), "no page written")
				return
			}
			require.NoError(t, err, "the page gate must let this fixture's page through")
			// While the vendored viewer is the one the fixture's page was
			// emitted with, re-emitting must give that page byte for byte:
			// the path re-emitted here is the one capsulectl took.
			if committed, err := os.ReadFile(filepath.Join(dir, "page.html")); err == nil && sameViewer(committed) {
				require.Equal(t, string(committed), page, "the re-emitted page differs from the one capsulectl emitted")
			}
			if chrome == nil {
				return
			}
			pagePath := filepath.Join(t.TempDir(), "page.html")
			require.NoError(t, os.WriteFile(pagePath, []byte(page), 0o600))
			url := "file://" + pagePath
			for _, view := range presentationViews {
				opened := chrome.open(t, url, view)
				var width struct {
					ScrollWidth int      `json:"scroll_width"`
					Width       int      `json:"width"`
					Over        []string `json:"over"`
				}
				opened.eval(t, overflowScript, &width)
				// A listed view may overflow (how much depends on the
				// platform's fonts: the outcome page overflows by 25px with
				// macOS fonts and fits with the CI runner's); any other must not.
				if !knownToOverflow(knownOverflow, entry.Name(), view.name) {
					assert.LessOrEqual(t, width.ScrollWidth, width.Width, "%s: the page is %dpx wide in a %dpx viewport (%v)", view.name, width.ScrollWidth, width.Width, width.Over)
				}
				if renders != "" {
					ext := ".png"
					if view.media == "print" {
						ext = ".pdf"
					}
					require.NoError(t, os.MkdirAll(renders, 0o755))
					opened.capture(t, view, filepath.Join(renders, entry.Name()+"-"+view.name+ext))
				}
				if view.name != "1280" {
					continue
				}
				var got presentationSnapshot
				opened.eval(t, snapshotScript, &got)
				snapshotPath := filepath.Join(dir, "snapshot.json")
				if update {
					writeSnapshotFile(t, snapshotPath, got)
					continue
				}
				var want presentationSnapshot
				readJSONFile(t, snapshotPath, &want)
				assert.Equal(t, want.Verification, got.Verification, "verification state")
				assert.Equal(t, want.Views, got.Views, "views")
				assert.Equal(t, want.EvidenceIDs, got.EvidenceIDs, "evidence identifiers")
				assertSameWords(t, want.Text, got.Text)
			}
		})
	}
	require.GreaterOrEqual(t, seen, 6, "the six presentation fixtures")
}

// rerenderPresentationPage emits the fixture's page from its bundle the way
// capsulectl does: disclose/bundle --html (the page gate, then the vendored
// viewer), deal report --html, or report build.
func rerenderPresentationPage(t *testing.T, dir string, meta presentationMeta, bundle map[string]interface{}) (string, error) {
	t.Helper()
	switch meta.Rerender {
	case "page":
		if err := pageGate(bundle); err != nil {
			return "", err
		}
		return emitter.EmitEvidenceGraphHTML(bundle, evidenceGraphIIFE)
	case "deal-report":
		return dealReportHTML(bundle, dealNotCountersigned())
	case "report-build":
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		out := filepath.Join(t.TempDir(), "page.html")
		if _, err := invoke(t, "", "report", "build", "--bundle", filepath.Join(dir, "bundle.json"), "--card", meta.Card, "--out", out); err != nil {
			return "", err
		}
		page, err := os.ReadFile(out)
		return string(page), err
	}
	t.Fatalf("unknown rerender %q", meta.Rerender)
	return "", nil
}

// knownToOverflow: known-overflow.json lists this fixture's view as one
// that overflowed when the goldens were taken, so it may still.
func knownToOverflow(known map[string]any, fixture, view string) bool {
	views, _ := known[fixture].([]any)
	for _, v := range views {
		if v == view {
			return true
		}
	}
	return false
}

// sameViewer: the page embeds the viewer this build vendors.
func sameViewer(page []byte) bool {
	sum := sha256.Sum256(evidenceGraphIIFE)
	return strings.Contains(string(page), string(evidenceGraphIIFE)) && hex.EncodeToString(sum[:]) == evidenceGraphIIFEDigest
}

func assertSameWords(t *testing.T, want, got []string) {
	t.Helper()
	w := strings.Fields(strings.Join(want, " "))
	g := strings.Fields(strings.Join(got, " "))
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			lo := max(0, i-12)
			t.Errorf("the page's text differs at word %d:\n want ...%s\n  got ...%s", i,
				strings.Join(w[lo:min(len(w), i+12)], " "), strings.Join(g[lo:min(len(g), i+12)], " "))
			return
		}
	}
	if len(w) != len(g) {
		t.Errorf("the page's text has %d words, want %d", len(g), len(w))
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, out))
}

func writeSnapshotFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o644))
}
