package cli

// dealAttribute is one attribute of what is about to happen, and its value.
type DealAttribute struct {
	Field string `json:"field"`
	Label string `json:"label"`
	Value string `json:"value"`
}

// commercialBoundsKind names the private document a floor lives in: sealed
// records carry only its salted commitment (bounds_commitment).
const CommercialBoundsKind = "commercial-bounds/v0"
