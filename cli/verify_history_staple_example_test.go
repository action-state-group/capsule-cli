package cli_test

import (
	"errors"
	"fmt"
	"os"

	"github.com/action-state-group/capsule-cli/cli"
)

// A caller outside the package reads a staple's outcome, the checks it
// could not run and each card's derivation.
func ExampleStapleVerdict() {
	raw, err := os.ReadFile("testdata/goose-commitment-v1/commitment.json")
	if err != nil {
		panic(err)
	}
	got, err := cli.StapleVerdict(raw, []string{"8c0c0699a2514ab17971f9596d05cd2ef121f4b21f5aadef34d42edb5a018088"}, nil)
	if err != nil {
		panic(err)
	}
	switch {
	case got.Outcome == nil:
		fmt.Println("accepted")
	case errors.Is(got.Outcome, cli.ErrHistoryAnswersNoRequest):
		fmt.Println("verified, answers no request")
	case errors.Is(got.Outcome, cli.ErrPartial):
		fmt.Println("not verified:", got.NotRun, "not checked")
	default:
		fmt.Println("not accepted:", got.Report.Findings)
	}
	fmt.Println(got.Derivations, got.Report.Staples[0].Properties["request_binding"])
	// Output:
	// not verified: [checkpoint_key] not checked
	// [history_card/1] NOT_PRESENT
}
