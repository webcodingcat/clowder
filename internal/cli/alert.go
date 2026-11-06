package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
)

func newAlert(g *globals) *cobra.Command {
	alert := &cobra.Command{Use: "alert", Short: "The cluster's alerts"}
	alert.AddCommand(&cobra.Command{
		Use:   "test",
		Short: "Send a test alert to every channel of the configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := g.connect()
			if err != nil {
				return err
			}
			var results []api.AlertTestResult
			if err := client.Act(cmd.Context(), "/v1/alerts/test", nil, &results); err != nil {
				return err
			}
			return printAlertTest(cmd.OutOrStdout(), results)
		},
	})
	return alert
}

// printAlertTest shows whether each channel accepted the test alert. It
// fails if one did not.
func printAlertTest(out io.Writer, results []api.AlertTestResult) error {
	failed := 0
	for _, result := range results {
		outcome := "sent"
		if result.Error != "" {
			outcome = "failed: " + result.Error
			failed++
		}
		if _, err := fmt.Fprintf(out, "%s: %s\n", result.Channel, outcome); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d channels did not take the test alert", failed, len(results))
	}
	return nil
}

// printAlerts shows the open alerts and the messages that the channels
// have not accepted yet.
func printAlerts(out io.Writer, alerts []api.AlertInfo, failures []api.AlertFailure, failuresErr error) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(alerts) == 0 {
		_, _ = fmt.Fprintln(table, "\nno open alert") // tabwriter reports errors on Flush
	} else {
		_, _ = fmt.Fprintln(table, "\nSEVERITY\tSINCE\tKEY\tALERT")
		for _, alert := range alerts {
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", alert.Severity, alert.Since.Local().Format(time.DateTime),
				alert.Key, alert.Subject)
		}
	}
	var statusError *api.StatusError
	switch {
	case errors.As(failuresErr, &statusError):
		_, _ = fmt.Fprintf(table, "\nfailed deliveries unknown: %s\n", statusError.Message)
	case failuresErr != nil:
		return failuresErr
	case len(failures) > 0:
		_, _ = fmt.Fprintln(table, "\nCHANNEL\tALERT\tEVENT\tATTEMPTS\tLAST ERROR")
		for _, failure := range failures {
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\n", failure.Channel, failure.Key, failure.Event,
				failure.Attempts, failure.LastError)
		}
	}
	return table.Flush()
}
