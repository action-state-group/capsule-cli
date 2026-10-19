package cli

// A deal receipt is the user's file. `deal report --html` writes the user's
// own copy (audience keep: nothing withheld). `--share counterparty|adjudicator`
// writes a copy for someone else, and sharing is a disclose act: the copy
// withholds what that audience must not get, and a disclosure record naming
// what was shared, with whom, in what mode and what was withheld is sealed
// onto the deal's disclosure log BEFORE the file is written. Nothing is
// hosted; the copy is a local file the user hands over.
const (
	DealAudienceKeep         = "keep"
	DealAudienceCounterparty = "counterparty"
	DealAudienceAdjudicator  = "adjudicator"
)
