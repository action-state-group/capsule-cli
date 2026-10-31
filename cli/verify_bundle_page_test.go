package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bundlePage(json string) []byte {
	return []byte(`<!doctype html><html><body><div id="app"></div>
<script type="application/json">` + json + `</script>
<script>window.__BUNDLE__ = JSON.parse(document.currentScript.previousElementSibling.textContent);</script>
</body></html>`)
}

// verify --bundle reads the bundle from the page's JSON data element
// (capsule-viewer#109), as data: a "__proto__" member stays a member.
func TestReceiptBundleJSONReadsTheJSONElement(t *testing.T) {
	json := `{"bundle_kind":"evidence-bundle/v2","__proto__":{"verdict":"VALID"},"note":"a ` + "\\u003c" + `/script` + "\\u003e" + ` inside"}`
	got, ok, err := receiptBundleJSON(bundlePage(json))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, json, string(got))
	value, err := DecodeBundleJSON(got)
	require.NoError(t, err)
	proto, present := value["__proto__"].(map[string]interface{})
	require.True(t, present, "the member survives as a member")
	assert.Equal(t, "VALID", proto["verdict"])
}

func TestReceiptBundleJSONFailsClosed(t *testing.T) {
	t.Run("a page in the old format is named as such, not read", func(t *testing.T) {
		old := []byte(`<!doctype html><script>window.__BUNDLE__ = {"bundle_kind":"evidence-bundle/v2"};</script>`)
		_, ok, err := receiptBundleJSON(old)
		assert.False(t, ok)
		require.ErrorIs(t, err, ErrInput)
		assert.ErrorContains(t, err, "old format")
	})
	t.Run("two bundle elements are not guessed between", func(t *testing.T) {
		two := append(bundlePage(`{"a":1}`), []byte(`<script type="application/json">{"b":2}</script>`)...)
		_, ok, err := receiptBundleJSON(two)
		assert.False(t, ok)
		assert.ErrorContains(t, err, "more than one bundle element")
	})
	t.Run("an element that is never closed", func(t *testing.T) {
		_, ok, err := receiptBundleJSON([]byte(`<!doctype html><script type="application/json">{"a":1}`))
		assert.False(t, ok)
		assert.ErrorContains(t, err, "never closed")
	})
	t.Run("a page with no bundle, and a file that is not a page, are not these", func(t *testing.T) {
		for _, raw := range []string{`<!doctype html><p>nothing here</p>`, `{"bundle_kind":"evidence-bundle/v2"}`} {
			got, ok, err := receiptBundleJSON([]byte(raw))
			assert.NoError(t, err)
			assert.False(t, ok)
			assert.Nil(t, got)
		}
	})
}
