package reap

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/audit"
)

// RenderJSON writes the plan as indented JSON.
func RenderJSON(w io.Writer, plan Plan) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(plan)
}

// RenderTable writes the candidates and a line explaining what was kept.
// dryRun only changes the wording of the summary.
func RenderTable(w io.Writer, plan Plan, dryRun bool) error {
	verb := "To delete"
	if dryRun {
		verb = "Would delete (dry run)"
	}
	if len(plan.Candidates) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAMESPACE\tJOB\tOWNER\tSTATUS\tCOMPLETED\tAGE")
		for _, c := range plan.Candidates {
			owner := c.Owner
			if owner == "" {
				owner = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				c.Namespace, c.Name, owner, c.Status,
				c.CompletionTime.UTC().Format(time.RFC3339), c.Age)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	_, err := fmt.Fprintf(w,
		"%s: %d of %d finished (total %d). Kept: %d newest per CronJob, %d within %s, %d other owner, %d unknown age, %d not selected, %d excluded.\n",
		verb, len(plan.Candidates), plan.FinishedJobs, plan.TotalJobs,
		plan.KeptLast, plan.WithinWindow, audit.HumanDuration(time.Duration(plan.OlderThanSeconds)*time.Second),
		plan.OtherOwner, plan.UnknownAge, plan.NotSelected, plan.Excluded)
	return err
}
