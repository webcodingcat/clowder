package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
)

func newVolume(g *globals) *cobra.Command {
	volume := &cobra.Command{
		Use:   "volume",
		Short: "Repair a volume's copies (apply, get and delete manage volumes)",
	}
	volume.AddCommand(newResolveSplitBrain(g))
	return volume
}

func newResolveSplitBrain(g *globals) *cobra.Command {
	var selected selectFlags
	var keep string
	command := &cobra.Command{
		Use:   "resolve-splitbrain [<namespace>/]<volume> --keep <host>",
		Short: "Connect a volume's copies again after a split brain, with the data of one",
		Long: "After a split brain (the alert split-brain), the volume's copies changed apart and DRBD\n" +
			"does not connect them again by itself. Look at the copies, then name the host whose copy\n" +
			"keeps its data: the copies on the other hosts give up their changes since the split and\n" +
			"resync from it. The volume's stack must run on that host, or not at all.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volume, err := selected.resolve("volume", args[0])
			if err != nil {
				return err
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			path := "/v1/volumes/" + url.PathEscape(volume.namespace) + "/" + url.PathEscape(volume.name) +
				"/resolve-splitbrain"
			var answer api.ResolveAnswer
			if err := client.Act(cmd.Context(), path, api.ResolveRequest{Keep: keep}, &answer); err != nil {
				return err
			}
			for _, resolved := range answer.Hosts {
				what := "nothing to resolve: no peer was StandAlone"
				switch {
				case resolved.Connected && resolved.Discarded:
					what = "gave its changes up and connects again"
				case resolved.Connected:
					what = "kept its data and connects again"
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", resolved.Host, what); err != nil {
					return err
				}
			}
			return nil
		},
	}
	selected.register(command, false)
	command.Flags().StringVar(&keep, "keep", "", "the host whose copy keeps its data")
	_ = command.MarkFlagRequired("keep") // the flag exists; this cannot fail
	return command
}
