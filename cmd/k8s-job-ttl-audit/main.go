// Command k8s-job-ttl-audit audits Kubernetes Jobs for a missing or too-long
// ttlSecondsAfterFinished so finished Jobs and their Pods stop accumulating.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

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
	root.AddCommand(newScanCmd())
	return root
}

type scanFlags struct {
	kubeconfig    string
	kubecontext   string
	namespace     string
	allNamespaces bool
	maxTTL        time.Duration
	output        string
	input         string
	failOn        string
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
	fl.StringVar(&f.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	fl.StringVar(&f.kubecontext, "context", "", "kubeconfig context to use (default: current)")
	fl.StringVarP(&f.namespace, "namespace", "n", "", "namespace to scan (default: current context namespace)")
	fl.BoolVarP(&f.allNamespaces, "all-namespaces", "A", false, "scan all namespaces")
	fl.DurationVar(&f.maxTTL, "max-ttl", 0, "flag finished Jobs whose TTL exceeds this (e.g. 24h); 0 flags only missing TTL")
	fl.StringVarP(&f.output, "output", "o", "table", "output format: table | json")
	fl.StringVar(&f.input, "input", "", "read Jobs+Pods from a JSON/YAML dump instead of a live cluster (offline)")
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

	var jobs []audit.Job
	var pods []audit.Pod

	if f.input != "" {
		data, err := os.ReadFile(f.input)
		if err != nil {
			return fmt.Errorf("read --input: %w", err)
		}
		jobs, pods, err = audit.ParseInput(data)
		if err != nil {
			return err
		}
	} else {
		ns := f.namespace
		if f.allNamespaces {
			ns = ""
		}
		c, err := cluster.New(f.kubeconfig, f.kubecontext)
		if err != nil {
			return err
		}
		jobs, pods, err = c.Fetch(ctx, ns)
		if err != nil {
			return err
		}
	}

	rep := audit.Classify(jobs, pods, audit.Options{MaxTTL: f.maxTTL})

	var err error
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
