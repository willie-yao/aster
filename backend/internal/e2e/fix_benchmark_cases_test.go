package e2e

import "strings"

const fixBenchmarkFixtureRoot = "backend/internal/e2e/testdata/fixbench"

type fixBenchmarkCase struct {
	Name                string
	Dir                 string
	Instruction         string
	RequiredFiles       []string
	RegressionTestFiles []string
	ReferenceFiles      map[string]string
	Module              string
	VerifierSource      string
}

func fixBenchmarkCases() []fixBenchmarkCase {
	return []fixBenchmarkCase{
		{
			Name:        "route-table-defaulting",
			Dir:         fixBenchmarkFixtureRoot + "/route_table",
			Instruction: `Default an empty control-plane route table name to the node route table name. Preserve an explicitly configured control-plane route table and leave an empty node route table unchanged. Add focused tests. Change only default.go and default_test.go, then run the package tests.`,
			RequiredFiles: []string{
				fixBenchmarkFixtureRoot + "/route_table/default.go",
				fixBenchmarkFixtureRoot + "/route_table/default_test.go",
			},
			RegressionTestFiles: []string{fixBenchmarkFixtureRoot + "/route_table/default_test.go"},
			ReferenceFiles: map[string]string{
				fixBenchmarkFixtureRoot + "/route_table/default.go": `package routetable

// NetworkSpec contains the route table names used by the control-plane and node subnets.
type NetworkSpec struct {
	ControlPlaneRouteTable string
	NodeRouteTable         string
}

// DefaultControlPlaneRouteTable applies network defaults without replacing explicit values.
func DefaultControlPlaneRouteTable(spec *NetworkSpec) {
	if spec == nil || spec.ControlPlaneRouteTable != "" {
		return
	}
	spec.ControlPlaneRouteTable = spec.NodeRouteTable
}
`,
				fixBenchmarkFixtureRoot + "/route_table/default_test.go": `package routetable

import "testing"

func TestDefaultControlPlaneRouteTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *NetworkSpec
		want string
	}{
		{name: "inherits node route table", spec: &NetworkSpec{NodeRouteTable: "node"}, want: "node"},
		{name: "preserves explicit value", spec: &NetworkSpec{ControlPlaneRouteTable: "control-plane", NodeRouteTable: "node"}, want: "control-plane"},
		{name: "leaves both empty", spec: &NetworkSpec{}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			DefaultControlPlaneRouteTable(tc.spec)
			if tc.spec.ControlPlaneRouteTable != tc.want {
				t.Fatalf("control-plane route table = %q, want %q", tc.spec.ControlPlaneRouteTable, tc.want)
			}
		})
	}
	DefaultControlPlaneRouteTable(nil)
}
`,
			},
			Module: "fixbench/route_table",
			VerifierSource: `package main

import (
	"fmt"

	routetable "fixbench/route_table"
)

func verify() error {
	spec := &routetable.NetworkSpec{NodeRouteTable: "shared-node-table"}
	routetable.DefaultControlPlaneRouteTable(spec)
	if spec.ControlPlaneRouteTable != "shared-node-table" || spec.NodeRouteTable != "shared-node-table" {
		return fmt.Errorf("shared route tables = %#v", spec)
	}
	explicit := &routetable.NetworkSpec{ControlPlaneRouteTable: "dedicated", NodeRouteTable: "shared"}
	routetable.DefaultControlPlaneRouteTable(explicit)
	if explicit.ControlPlaneRouteTable != "dedicated" || explicit.NodeRouteTable != "shared" {
		return fmt.Errorf("explicit route tables = %#v", explicit)
	}
	empty := &routetable.NetworkSpec{}
	routetable.DefaultControlPlaneRouteTable(empty)
	if empty.ControlPlaneRouteTable != "" || empty.NodeRouteTable != "" {
		return fmt.Errorf("empty route tables = %#v", empty)
	}
	routetable.DefaultControlPlaneRouteTable(nil)
	return nil
}
`,
		},
		{
			Name:        "retry-validation",
			Dir:         fixBenchmarkFixtureRoot + "/retry",
			Instruction: `Reject negative retry counts without changing the Parse function signature. Preserve zero, positive values, and the existing invalid-string error behavior. Add a focused negative-value test. Change only retry.go and retry_test.go, then run the package tests.`,
			RequiredFiles: []string{
				fixBenchmarkFixtureRoot + "/retry/retry.go",
				fixBenchmarkFixtureRoot + "/retry/retry_test.go",
			},
			RegressionTestFiles: []string{fixBenchmarkFixtureRoot + "/retry/retry_test.go"},
			ReferenceFiles: map[string]string{
				fixBenchmarkFixtureRoot + "/retry/retry.go": `package retry

import (
	"fmt"
	"strconv"
)

// Parse converts a configured retry count into an integer.
func Parse(value string) (int, error) {
	retries, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse retries: %w", err)
	}
	if retries < 0 {
		return 0, fmt.Errorf("retries must be non-negative")
	}
	return retries, nil
}
`,
				fixBenchmarkFixtureRoot + "/retry/retry_test.go": `package retry

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{value: "0", want: 0},
		{value: "3", want: 3},
	} {
		got, err := Parse(tc.value)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.value, err)
		}
		if got != tc.want {
			t.Fatalf("Parse(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
	for _, value := range []string{"many", "-1"} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", value)
		}
	}
}
`,
			},
			Module: "fixbench/retry",
			VerifierSource: `package main

import (
	"fmt"

	retry "fixbench/retry"
)

func verify() error {
	if _, err := retry.Parse("-7"); err == nil {
		return fmt.Errorf("negative retries were accepted")
	}
	for value, want := range map[string]int{"0": 0, "8": 8} {
		got, err := retry.Parse(value)
		if err != nil || got != want {
			return fmt.Errorf("Parse(%q) = %d, %v", value, got, err)
		}
	}
	if _, err := retry.Parse("many"); err == nil {
		return fmt.Errorf("nonnumeric retries were accepted")
	}
	return nil
}
`,
		},
		{
			Name:        "generated-manifest-sync",
			Dir:         fixBenchmarkFixtureRoot + "/generated_manifest",
			Instruction: `Add activeDeadlineSeconds: 900 to the generated CronJob job spec and update the committed golden manifest. Preserve restartPolicy: Never and every unrelated field. Change exactly generator.go and testdata/golden.yaml, then run the package tests.`,
			RequiredFiles: []string{
				fixBenchmarkFixtureRoot + "/generated_manifest/generator.go",
				fixBenchmarkFixtureRoot + "/generated_manifest/testdata/golden.yaml",
			},
			ReferenceFiles: map[string]string{
				fixBenchmarkFixtureRoot + "/generated_manifest/generator.go": `package manifest

import "fmt"

// RenderCronJob renders the generated CronJob fixture.
func RenderCronJob(name string) string {
	return fmt.Sprintf(` + "`" + `apiVersion: batch/v1
kind: CronJob
metadata:
  name: %s
spec:
  jobTemplate:
    spec:
      activeDeadlineSeconds: 900
      template:
        spec:
          restartPolicy: Never
` + "`" + `, name)
}
`,
				fixBenchmarkFixtureRoot + "/generated_manifest/testdata/golden.yaml": `apiVersion: batch/v1
kind: CronJob
metadata:
  name: fixture
spec:
  jobTemplate:
    spec:
      activeDeadlineSeconds: 900
      template:
        spec:
          restartPolicy: Never
`,
			},
			Module: "fixbench/generated_manifest",
			VerifierSource: `package main

import (
	"fmt"

	manifest "fixbench/generated_manifest"
)

func verify() error {
	got := manifest.RenderCronJob("hidden")
	want := ` + "`" + `apiVersion: batch/v1
kind: CronJob
metadata:
  name: hidden
spec:
  jobTemplate:
    spec:
      activeDeadlineSeconds: 900
      template:
        spec:
          restartPolicy: Never
` + "`" + `
	if got != want {
		return fmt.Errorf("generated manifest differs:\n%s", got)
	}
	return nil
}
`,
		},
	}
}

func fixBenchmarkCaseByName(name string) (fixBenchmarkCase, bool) {
	for _, benchmarkCase := range fixBenchmarkCases() {
		if benchmarkCase.Name == strings.TrimSpace(name) {
			return benchmarkCase, true
		}
	}
	return fixBenchmarkCase{}, false
}
