package cli

import (
	"regexp"
)

// A sale is one item a user sells to one of several buyers. It is its own
// log: a root (what is for sale, the seller's request, and the item
// reference by commitment) and the sale's one task authority. Each buyer's
// negotiation is its own deal, a thread opened under the sale with
// `deal open --sale`: the thread is sealed under the sale's task authority
// (its task authority commits to that one's digest under its own nonce,
// sale_authority_commitment), and each of its checks seals the item
// reference as a commitment salted per check, so no buyer's copy carries the
// reference or anything equal across threads.
// The plain reference goes only to the profile's own rules checker
// (item_ref), which is how it holds a sale to one accepted commitment.

var SaleIDPattern = regexp.MustCompile(`^sale-[0-9a-f]{16}$`)
