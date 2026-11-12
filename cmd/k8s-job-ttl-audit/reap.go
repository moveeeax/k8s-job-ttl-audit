package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/reap"
)

type reapFlags struct {
	sourceFlags
	olderThan   time.Duration
	keepLast    int
	selector    string
	exclude     []string
	dryRun      bool
	output      string
	interval    time.Duration
	metricsAddr string
}

func newReapCmd() *cobra.Command {
	f := &reapFlags{}
	cmd := &cobra.Command{
		Use:   "reap",
		Short: "Delete finished Jobs older than --older-than, keeping the newest --keep-last per CronJob",
		Long: `Delete finished (Complete or Failed) Jobs that finished longer ago than
--older-than, keeping the newest --keep-last Jobs of every owning CronJob.
Each Job is deleted with background propagation, so its Pods go with it.

Runs as a dry run by default and only lists what it would delete. Pass
--dry-run=false to delete. Jobs managed by a controller other than a CronJob
and Jobs with no completion time are never deleted.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReap(cmd.Context(), f, os.Stdout)
		},
	}
	fl := cmd.Flags()
	f.register(fl)
	fl.DurationVar(&f.olderThan, "older-than", 168*time.Hour, "retention window: reap Jobs that finished longer ago than this")
	fl.IntVar(&f.keepLast, "keep-last", 5, "never reap the newest N finished Jobs of each CronJob")
	fl.StringVarP(&f.selector, "selector", "l", "", "only reap Jobs matching this label selector (e.g. team=data,tier!=prod)")
	fl.StringSliceVar(&f.exclude, "exclude", nil, "never reap these Jobs: name or namespace/name, globs allowed (repeatable)")
	fl.BoolVar(&f.dryRun, "dry-run", true, "only list what would be deleted; --dry-run=false deletes")
	fl.StringVarP(&f.output, "output", "o", "table", "output format: table | json")
	fl.DurationVar(&f.interval, "interval", 0, "repeat the sweep at this interval until interrupted; 0 runs once")
	fl.StringVar(&f.metricsAddr, "metrics-addr", "", "serve Prometheus metrics on this address (e.g. :9090) while looping")
	return cmd
}

func (f *reapFlags) validate() (labels.Selector, error) {
	if f.output != "table" && f.output != "json" {
		return nil, fmt.Errorf("invalid --output %q: want table or json", f.output)
	}
	if f.olderThan <= 0 {
		return nil, fmt.Errorf("invalid --older-than %s: must be > 0", f.olderThan)
	}
	if f.keepLast < 0 {
		return nil, fmt.Errorf("invalid --keep-last %d: must be >= 0", f.keepLast)
	}
	if f.interval < 0 {
		return nil, fmt.Errorf("invalid --interval %s: must be >= 0", f.interval)
	}
	if f.metricsAddr != "" && f.interval == 0 {
		return nil, errors.New("--metrics-addr needs --interval: a single run exits before it can be scraped")
	}
	if f.input != "" && !f.dryRun {
		return nil, errors.New("--input is an offline dump: it can only be used with --dry-run")
	}
	if f.input != "" && f.interval > 0 {
		return nil, errors.New("--input cannot be combined with --interval")
	}
	if err := reap.ValidateExclude(f.exclude); err != nil {
		return nil, err
	}
	sel := labels.Everything()
	if f.selector != "" {
		s, err := labels.Parse(f.selector)
		if err != nil {
			return nil, fmt.Errorf("invalid --selector: %w", err)
		}
		sel = s
	}
	return sel, nil
}

func runReap(ctx context.Context, f *reapFlags, out io.Writer) error {
	sel, err := f.validate()
	if err != nil {
		return err
	}
	opts := reap.Options{
		OlderThan: f.olderThan,
		KeepLast:  f.keepLast,
		Selector:  sel,
		Exclude:   f.exclude,
	}

	if f.interval == 0 {
		return reapOnce(ctx, f, opts, out, nil)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := reap.NewMetrics()
	if f.metricsAddr != "" {
		ln, err := net.Listen("tcp", f.metricsAddr)
		if err != nil {
			return fmt.Errorf("listen --metrics-addr: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", m)
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
	}

	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		if err := reapOnce(ctx, f, opts, out, m); err != nil {
			// Keep looping: a transient API error should not stop the reaper.
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// reapOnce runs one sweep: list, plan, print, and delete unless dry-running.
func reapOnce(ctx context.Context, f *reapFlags, opts reap.Options, out io.Writer, m *reap.Metrics) (err error) {
	candidates := 0
	defer func() { m.ObserveRun(candidates, err != nil, float64(time.Now().Unix())) }()

	inv, client, err := f.load(ctx)
	if err != nil {
		return err
	}
	opts.Now = time.Now()
	plan := reap.PlanReap(inv.Jobs, opts)
	candidates = len(plan.Candidates)

	if f.output == "json" {
		err = reap.RenderJSON(out, plan)
	} else {
		err = reap.RenderTable(out, plan, f.dryRun)
	}
	if err != nil || f.dryRun {
		return err
	}

	res := reap.Execute(ctx, plan, client, m)
	fmt.Fprintf(os.Stderr, "Deleted %d Jobs, %d already gone, %d errors.\n", res.Deleted, res.Gone, len(res.Errors))
	if len(res.Errors) > 0 {
		return fmt.Errorf("%d of %d deletions failed: %w", len(res.Errors), len(plan.Candidates), errors.Join(res.Errors...))
	}
	return nil
}
