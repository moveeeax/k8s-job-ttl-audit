// Command k8s-job-ttl-audit audits Kubernetes Jobs for a missing or too-long
// ttlSecondsAfterFinished so finished Jobs and their Pods stop accumulating.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/audit"
	"github.com/moveeeax/k8s-job-ttl-audit/internal/cluster"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "k8s-job-ttl-audit",
		Short:         "Audit Kubernetes Jobs for missing/oversized ttlSecondsAfterFinished",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.AddCommand(newScanCmd(), newReapCmd())
	return root
}

// sourceFlags select where Jobs come from: a live cluster or an --input dump.
type sourceFlags struct {
	kubeconfig    string
	kubecontext   string
	namespace     string
	allNamespaces bool
	input         string
}

func (s *sourceFlags) register(fl *pflag.FlagSet) {
	fl.StringVar(&s.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	fl.StringVar(&s.kubecontext, "context", "", "kubeconfig context to use (default: current)")
	fl.StringVarP(&s.namespace, "namespace", "n", "", "namespace to scan (default: current context namespace)")
	fl.BoolVarP(&s.allNamespaces, "all-namespaces", "A", false, "scan all namespaces")
	fl.StringVar(&s.input, "input", "", "read Jobs, Pods and CronJobs from a JSON/YAML dump instead of a live cluster (offline)")
}

// load reads the inventory from --input or the cluster. client is nil in
// offline mode.
func (s *sourceFlags) load(ctx context.Context) (audit.Inventory, *cluster.Client, error) {
	if s.input != "" {
		data, err := os.ReadFile(s.input)
		if err != nil {
			return audit.Inventory{}, nil, fmt.Errorf("read --input: %w", err)
		}
		inv, err := audit.ParseInput(data)
		return inv, nil, err
	}
	ns := s.namespace
	if s.allNamespaces {
		ns = ""
	}
	c, err := cluster.New(s.kubeconfig, s.kubecontext)
	if err != nil {
		return audit.Inventory{}, nil, err
	}
	inv, err := c.Fetch(ctx, ns)
	return inv, c, err
}

type scanFlags struct {
	sourceFlags
	maxTTL     time.Duration
	maxHistory int
	output     string
	failOn     string
}

func newScanCmd() *cobra.Command {
	f := &scanFlags{}
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan Jobs and report stale ones (no TTL or TTL > --max-ttl)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runScan(cmd.Context(), f)
		},
	}
	fl := cmd.Flags()
	f.register(fl)
	fl.DurationVar(&f.maxTTL, "max-ttl", 0, "flag finished Jobs whose TTL exceeds this (e.g. 24h); 0 flags only missing TTL")
	fl.IntVar(&f.maxHistory, "max-history", 10, "do not flag a TTL-less CronJob Job when the CronJob history limit for its state is at most this")
	fl.StringVarP(&f.output, "output", "o", "table", "output format: table | json")
	fl.StringVar(&f.failOn, "fail-on", "", "exit non-zero when findings exist: stale")
	return cmd
}

func runScan(ctx context.Context, f *scanFlags) error {
	if f.output != "table" && f.output != "json" {
		return fmt.Errorf("invalid --output %q: want table or json", f.output)
	}
	if f.failOn != "" && f.failOn != "stale" {
		return fmt.Errorf("invalid --fail-on %q: want stale", f.failOn)
	}
	if f.maxHistory < 0 {
		return fmt.Errorf("invalid --max-history %d: must be >= 0", f.maxHistory)
	}

	inv, _, err := f.load(ctx)
	if err != nil {
		return err
	}

	rep := audit.Classify(inv.Jobs, inv.Pods, audit.Options{
		MaxTTL:         f.maxTTL,
		CronJobs:       inv.CronJobs,
		CronJobsListed: inv.CronJobsListed,
		MaxHistory:     f.maxHistory,
	})

	if f.output == "json" {
		err = audit.RenderJSON(os.Stdout, rep)
	} else {
		err = audit.RenderTable(os.Stdout, rep)
	}
	if err != nil {
		return err
	}

	if f.failOn == "stale" && rep.StaleJobs > 0 {
		os.Exit(1)
	}
	return nil
}
