package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// RenderJSON writes the report as stable, indented JSON.
func RenderJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// RenderTable writes a human-readable table of findings and a summary.
func RenderTable(w io.Writer, rep Report) error {
	if rep.StaleJobs == 0 {
		_, err := fmt.Fprintf(w, "No stale Jobs found (%d finished of %d total%s).\n",
			rep.FinishedJobs, rep.TotalJobs, boundedStr(rep))
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tJOB\tSTATUS\tREASON\tAGE\tTTL\tPODS\tOWNER\tHISTORY")
	for _, f := range rep.Findings {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			f.Namespace, f.Name, f.Status, f.Reason,
			f.CompletionAge, ttlStr(f.TTLSeconds), f.LeftoverPods, dash(f.Owner),
			historyStr(f.HistoryLimit))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Stale Jobs: %d   Leftover Pods: %d   (finished %d / total %d%s)\n",
		rep.StaleJobs, rep.LeftoverPods, rep.FinishedJobs, rep.TotalJobs, boundedStr(rep))

	if len(rep.ByNamespace) > 1 {
		fmt.Fprintln(w, "\nPer namespace:")
		nsw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(nsw, "  NAMESPACE\tSTALE\tPODS")
		for _, s := range rep.ByNamespace {
			fmt.Fprintf(nsw, "  %s\t%d\t%d\n", s.Namespace, s.StaleJobs, s.LeftoverPods)
		}
		nsw.Flush()
	}
	return nil
}

func ttlStr(ttl *int32) string {
	if ttl == nil {
		return "none"
	}
	return fmt.Sprintf("%ds", *ttl)
}

func historyStr(limit *int32) string {
	if limit == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *limit)
}

// boundedStr notes how many Jobs were skipped because their CronJob history
// limit already bounds them; empty when CronJobs were not listed.
func boundedStr(rep Report) string {
	if rep.MaxHistory == nil {
		return ""
	}
	return fmt.Sprintf(", %d bounded by CronJob history <= %d", rep.BoundedByHistory, *rep.MaxHistory)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
