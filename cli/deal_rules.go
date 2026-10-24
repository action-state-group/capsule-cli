package cli

// dealAttribute is one attribute of what is about to happen, and its value.
type DealAttribute struct {
	Field string `json:"field"`
	Label string `json:"label"`
	Value string `json:"value"`
	// Delegated marks the agent's pick of what the user handed it to
	// choose: its own, under the user's words, and never a pause.
	Delegated bool `json:"delegated,omitempty"`
}

// commercialBoundsKind names the private document a floor lives in: sealed
// records carry only its salted commitment (bounds_commitment).
const CommercialBoundsKind = "commercial-bounds/v0"
