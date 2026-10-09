package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verify --bundle prints the same bytes on every run: each disclosures array,
// the containing Bundle's and every composed member bundle's, is ordered by
// capsule id, then member, then status, whatever order the verifier listed
// them in.
func TestVerifyBundleDisclosuresAreInAStableOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, vector := range composedVectors(t) {
		t.Run(vector.ID, func(t *testing.T) {
			path := writeBundle(t, vector.Container)
			first, _ := invoke(t, "", "verify", "--bundle", path)
			for range 30 {
				again, _ := invoke(t, "", "verify", "--bundle", path)
				require.Equal(t, first, again, "the same bundle, the same bytes")
			}
			var result map[string]any
			require.NoError(t, json.Unmarshal([]byte(first), &result))
			for _, list := range disclosureLists(result) {
				for i := 1; i < len(list); i++ {
					assert.False(t, disclosureAfter(list[i-1], list[i]), "%v before %v", list[i-1], list[i])
				}
			}
		})
	}
}

// A record that withholds both eligible members lists both, input first.
func TestAssessBundleOrdersWithheldMembers(t *testing.T) {
	vector := composedVectors(t)[0]
	var withheld int
	for range 30 {
		_, report := bundleVerdict(vector.Container, nil)
		for _, list := range disclosureLists(roundTrip(t, report)) {
			for i := 1; i < len(list); i++ {
				require.False(t, disclosureAfter(list[i-1], list[i]), "%v before %v", list[i-1], list[i])
			}
			for _, d := range list {
				if d["status"] == "withheld" {
					withheld++
				}
			}
		}
	}
	require.Positive(t, withheld, "the vectors withhold members, so the order is exercised")
}

func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// disclosureLists is every disclosures array in a verify result: its own
// and, recursively, each composed member bundle's.
func disclosureLists(result map[string]any) [][]map[string]any {
	var lists [][]map[string]any
	var list []map[string]any
	for _, d := range result["disclosures"].([]any) {
		list = append(list, d.(map[string]any))
	}
	lists = append(lists, list)
	extensions, _ := result["extensions"].([]any)
	for _, x := range extensions {
		composed, _ := x.(map[string]any)["composed"].(map[string]any)
		members, _ := composed["members"].([]any)
		for _, m := range members {
			if bundle, ok := m.(map[string]any)["bundle"].(map[string]any); ok {
				lists = append(lists, disclosureLists(bundle)...)
			}
		}
	}
	return lists
}

func disclosureAfter(a, b map[string]any) bool {
	for _, k := range []string{"capsule_id", "member", "status"} {
		if a[k] != b[k] {
			return a[k].(string) > b[k].(string)
		}
	}
	return false
}
