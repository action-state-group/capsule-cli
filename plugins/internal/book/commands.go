package book

import (
	"fmt"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/spf13/cobra"
)

// Vendor is what both first-party plugins name in their metadata.
const Vendor = "action-state-group"

// BookMetadata is capsulectl-book's cli-plugin-metadata answer.
var BookMetadata = cli.PluginMetadata{Name: cli.BookPluginName, Vendor: Vendor, Subcommands: []string{"close", "reconcile", "request", "respond", "remote", "result", "report", "backfill", "migrate"}}

// BookCommands are capsulectl-book's own commands: `capsulectl book ...`.
// The binary also holds the core tree, which reaches book profiles through
// BookBackend.
func BookCommands() []*cobra.Command {
	remote := cli.RemoteBookCommand()
	request := bookRequestCommand(remote)
	respond := onProfiles(respondCommand(), func(p cli.Profile) error {
		if p.Type == "sqlite" {
			return cli.InputError("this is a deal profile: it answers with `capsulectl deal respond`")
		}
		return nil
	})
	return []*cobra.Command{closeCommand(), reconcileCommand(), request, respond, remote, resultCommands(), reportCommands(), backfillCommands(), migrateCommand()}
}

// BookAliases are `capsulectl book list|get|head`, which read another
// party's book before they moved under `remote` (capsule-cli#238). They sit
// beside BookCommands at the top of capsulectl-book, which the core reaches
// only through `capsulectl book`, so they never stand for the core's `get`.
func BookAliases() []*cobra.Command {
	var aliases []*cobra.Command
	for _, sub := range cli.RemoteBookCommand().Commands() {
		if sub.Name() != "request" {
			aliases = append(aliases, remoteAlias(sub.Name()))
		}
	}
	return aliases
}

// bookRequestCommand is `capsulectl book request` on the profile's own book.
// On a remote profile it is still the old `capsulectl book request`, which
// asked another party's book and is now `capsulectl book remote request`
// (capsule-cli#238): it says so and asks as before. The old form's --party
// is accepted for that; --request and --responder-key mean the same in both.
func bookRequestCommand(remote *cobra.Command) *cobra.Command {
	var ask func(*cobra.Command, []string) error
	for _, sub := range remote.Commands() {
		if sub.Name() == "request" {
			ask = sub.RunE
		}
	}
	cmd := requestCommand()
	cmd.Flags().String("party", "", "Deprecated with `capsulectl book request` on a remote profile: use `capsulectl book remote request --party`")
	_ = cmd.Flags().MarkHidden("party")
	own := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		p, err := cli.Selected(c)
		if err != nil {
			return err
		}
		switch p.Type {
		case cli.RemoteProfileType:
			fmt.Fprintf(c.ErrOrStderr(), "capsulectl: `capsulectl book request` on a remote profile is deprecated; use `capsulectl book remote request` (the alias is removed in %s)\n", cli.AliasRemovedIn)
			return ask(c, args)
		case "sqlite":
			return cli.InputError("this is a deal profile: its requests are `capsulectl deal request`")
		}
		if c.Flags().Changed("party") {
			return cli.InputError("--party asks another party's book: `capsulectl book remote request` on a remote profile")
		}
		return own(c, args)
	}
	return cmd
}

// remoteAlias is the old `capsulectl book <verb>` for reading another
// party's book, now `capsulectl book remote <verb>`.
func remoteAlias(verb string) *cobra.Command {
	return &cobra.Command{
		Use:                verb,
		Short:              fmt.Sprintf("Deprecated: use `capsulectl book remote %s`; removed in %s", verb, cli.AliasRemovedIn),
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Fprintf(c.ErrOrStderr(), "capsulectl: `capsulectl book %s` is deprecated; use `capsulectl book remote %s` (the alias is removed in %s)\n", verb, verb, cli.AliasRemovedIn)
			root := c.Root()
			root.SetArgs(append([]string{"remote", verb}, args...))
			return root.ExecuteContext(c.Context())
		},
	}
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

// migrateCommand is `capsulectl book migrate` (formerly `store migrate`):
// a book profile's pre-book cll.jsonl moves into its evidence book.
func migrateCommand() *cobra.Command {
	migrate := &cobra.Command{Use: "migrate", Short: "Move a book profile's pre-book cll.jsonl into its evidence book, once, in order (signs)", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := cli.Selected(c)
		if e != nil {
			return e
		}
		if !p.IsBook() || p.ReadOnly {
			return cli.InputError("book migrate applies to a writable book profile")
		}
		newLogID, _ := c.Flags().GetString("log-id")
		result, e := migrateStore(c.Context(), p, newLogID, bookNow())
		if e != nil {
			return e
		}
		return cli.Output(c, result)
	}}
	migrate.Flags().String("log-id", "", "New log_id for the book; required when the retired log was ever checkpointed")
	return migrate
}
