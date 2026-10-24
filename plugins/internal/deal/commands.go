package deal

import (
	"github.com/action-state-group/capsule-cli/cli"
	"github.com/spf13/cobra"
)

// Vendor is what both first-party plugins name in their metadata.
const Vendor = "action-state-group"

// DealMetadata is capsulectl-deal's cli-plugin-metadata answer.
var DealMetadata = cli.PluginMetadata{Name: cli.DealPluginName, Vendor: Vendor, Subcommands: []string{"init", "open", "check", "note", "close", "report", "export", "list", "history", "deadlines", "settlement", "audit-host", "reconcile", "countersign", "verify-email", "verify-object", "verifier-statement", "verify-statement", "tick", "checkpoint", "sale", "compose", "request", "respond", "terms-result"}}

// DealCommands are capsulectl-deal's own commands: `capsulectl deal ...`.
func DealCommands() []*cobra.Command {
	return dealCommands().Commands()
}

// DealGroup is the deal commands under one `deal` command, as the core tree
// holds a plugin's: what tests run `deal ...` against.
func DealGroup() *cobra.Command {
	return dealCommands()
}

// dealCommands' own request, respond and compose: a deal profile's copies.
func dealEvidenceCommands() []*cobra.Command {
	notDeal := func(p cli.Profile) error {
		if p.Type != "sqlite" {
			return cli.InputError("this is not a deal profile: an evidence book's requests are `capsulectl book request` and `capsulectl book respond`")
		}
		return nil
	}
	return []*cobra.Command{onProfiles(requestCommand(), notDeal), onProfiles(respondCommand(), notDeal), composeCommand()}
}

// onProfiles runs check on the selected profile before cmd's own RunE.
func onProfiles(cmd *cobra.Command, check func(cli.Profile) error) *cobra.Command {
	run := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		p, err := cli.Selected(c)
		if err != nil {
			return err
		}
		if err := check(p); err != nil {
			return err
		}
		return run(c, args)
	}
	return cmd
}

// InstallDealHooks gives the core tree the deal commands it reaches from
// its own: `bundle --deal`, `cll checkpoint cadence`, and the deal verbs
// `canary run` drives. Call it before the tree is built.
func InstallDealHooks() {
	cli.DealBundleRun = dealBundleRun
	cli.DealTickCommand = dealTickCommand
	cli.DealCommandGroup = DealGroup
	cli.ErrorPhase = dealErrorPhase
}
