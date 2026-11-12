package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func validReapFlags() *reapFlags {
	return &reapFlags{olderThan: 168 * time.Hour, keepLast: 5, dryRun: true, output: "table"}
}

func TestReapFlagValidation(t *testing.T) {
	cases := map[string]func(f *reapFlags){
		"bad output":           func(f *reapFlags) { f.output = "yaml" },
		"zero window":          func(f *reapFlags) { f.olderThan = 0 },
		"negative keep-last":   func(f *reapFlags) { f.keepLast = -1 },
		"metrics without loop": func(f *reapFlags) { f.metricsAddr = ":9090" },
		"input deletes":        func(f *reapFlags) { f.input = "dump.json"; f.dryRun = false },
		"input loops":          func(f *reapFlags) { f.input = "dump.json"; f.interval = time.Minute },
		"bad selector":         func(f *reapFlags) { f.selector = "a in (" },
		"bad exclude":          func(f *reapFlags) { f.exclude = []string{"a/b/c"} },
	}
	for name, mutate := range cases {
		f := validReapFlags()
		mutate(f)
		if _, err := f.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := validReapFlags().validate(); err != nil {
		t.Errorf("defaults rejected: %v", err)
	}
}

func TestReapDryRunFromFixture(t *testing.T) {
	f := validReapFlags()
	f.input = "../../examples/jobs-dump.json"
	f.keepLast = 1
	var out bytes.Buffer
	if err := runReap(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "Would delete (dry run): 2 of 5 finished") {
		t.Errorf("unexpected plan:\n%s", s)
	}
	for _, kept := range []string{"nightly-backup-28401", "hourly-sync-29299", "weekly-report-27990"} {
		if strings.Contains(s, kept) {
			t.Errorf("%s is the newest Job of its CronJob and must be kept:\n%s", kept, s)
		}
	}
}
