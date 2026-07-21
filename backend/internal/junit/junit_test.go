package junit

import (
	"os"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/junit.xml")
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	return data
}

func TestParse(t *testing.T) {
	data := loadFixture(t)
	cases, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if got := len(cases); got != 5 {
		t.Fatalf("expected 5 test cases, got %d", got)
	}

	// Failed test
	tc := cases[0]
	if tc.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", tc.Status)
	}
	if tc.DurationSeconds != 4600.00 {
		t.Errorf("expected duration 4600.00, got %f", tc.DurationSeconds)
	}
	if !strings.Contains(tc.Name, "HA cluster") {
		t.Errorf("unexpected name: %s", tc.Name)
	}
	if !strings.Contains(tc.FailureMessage, "Timed out after 3600.001s") {
		t.Errorf("failure message missing expected text: %s", tc.FailureMessage)
	}
	if !strings.Contains(tc.FailureBody, "Full Stack Trace") {
		t.Errorf("failure body missing stack trace: %s", tc.FailureBody)
	}
	if tc.FailureLocation == "" {
		t.Error("expected failure location to be extracted")
	}
	if tc.FailureLocURL == "" {
		t.Error("expected failure location URL to be generated")
	}

	// Skipped via <skipped> element.
	tc = cases[1]
	if tc.Status != "skipped" {
		t.Errorf("expected status 'skipped', got %q", tc.Status)
	}

	// Passed test
	tc = cases[2]
	if tc.Status != "passed" {
		t.Errorf("expected status 'passed', got %q", tc.Status)
	}
	if tc.DurationSeconds != 1234.56 {
		t.Errorf("expected duration 1234.56, got %f", tc.DurationSeconds)
	}

	// Skipped via status attr only.
	tc = cases[3]
	if tc.Status != "skipped" {
		t.Errorf("expected status 'skipped', got %q", tc.Status)
	}

	// Second suite
	tc = cases[4]
	if tc.Status != "passed" {
		t.Errorf("expected status 'passed', got %q", tc.Status)
	}
	if tc.DurationSeconds != 300.00 {
		t.Errorf("expected duration 300.00, got %f", tc.DurationSeconds)
	}
}

func TestExtractFailureLocation_Versioned(t *testing.T) {
	body := `[FAILED] some error
sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115
sigs.k8s.io/cluster-api-provider-azure/test/e2e/helpers.go:42`

	loc, url := ExtractFailureLocation(body)

	expectedLoc := "sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115"
	if loc != expectedLoc {
		t.Errorf("location = %q, want %q", loc, expectedLoc)
	}

	expectedURL := "https://github.com/kubernetes-sigs/cluster-api/blob/v1.12.3/test/framework/controlplane_helpers.go#L115"
	if url != expectedURL {
		t.Errorf("url = %q, want %q", url, expectedURL)
	}
}

func TestExtractFailureLocation_Unversioned(t *testing.T) {
	body := `sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:42`

	loc, url := ExtractFailureLocation(body)

	expectedLoc := "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:42"
	if loc != expectedLoc {
		t.Errorf("location = %q, want %q", loc, expectedLoc)
	}

	expectedURL := "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/test/e2e/azure_test.go#L42"
	if url != expectedURL {
		t.Errorf("url = %q, want %q", url, expectedURL)
	}
}

func TestExtractFailureLocation_MultipleLocations(t *testing.T) {
	body := `first location:
sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115
second location:
sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:42`

	loc, _ := ExtractFailureLocation(body)
	if !strings.Contains(loc, "controlplane_helpers.go:115") {
		t.Errorf("expected first location to be extracted, got %q", loc)
	}
}

func TestExtractFailureLocation_NoMatch(t *testing.T) {
	loc, url := ExtractFailureLocation("no go source references here")
	if loc != "" || url != "" {
		t.Errorf("expected empty results, got loc=%q url=%q", loc, url)
	}
}

func TestParse_EmptyXML(t *testing.T) {
	_, err := Parse([]byte(""))
	if err == nil {
		t.Error("expected error for empty XML")
	}
}

func TestParse_MalformedXML(t *testing.T) {
	_, err := Parse([]byte("<testsuites><broken"))
	if err == nil {
		t.Error("expected error for malformed XML")
	}
}

func TestParse_SingleTestSuite(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="suite1" tests="1" failures="0" errors="0" time="10.0">
  <testcase name="test1" classname="suite1" status="passed" time="10.0"/>
</testsuite>`

	cases, err := Parse([]byte(xml))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("expected 1 test case, got %d", len(cases))
	}
	if cases[0].Status != "passed" {
		t.Errorf("expected passed, got %q", cases[0].Status)
	}
}

// TestParse_SingleTestSuite_ZeroTestsAttr verifies tests="0" suites can still
// contain real testcases. The parser trusts elements, not the tests count.
func TestParse_SingleTestSuite_ZeroTestsAttr(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="ClusterLoaderV2" tests="0" failures="1" errors="0" time="100.0">
  <testcase name="load overall" classname="ClusterLoaderV2" time="50.0">
    <failure type="Failure">measurement failed</failure>
  </testcase>
  <testcase name="load: step 01" classname="ClusterLoaderV2" time="0.1"/>
</testsuite>`

	cases, err := Parse([]byte(xml))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("expected 2 test cases, got %d", len(cases))
	}
	if cases[0].Status != "failed" {
		t.Errorf("expected first case failed, got %q", cases[0].Status)
	}
	if cases[1].Status != "passed" {
		t.Errorf("expected second case passed, got %q", cases[1].Status)
	}
}

// TestParseFile stamps the source basename so the UI can disambiguate
// same-named tests across JUnit files.
func TestParseFile(t *testing.T) {
	data := loadFixture(t)
	cases, err := ParseFile(data, "junit_runner.xml")
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("expected at least one case")
	}
	for i, tc := range cases {
		if tc.JUnitFile != "junit_runner.xml" {
			t.Errorf("case[%d].JUnitFile = %q, want %q", i, tc.JUnitFile, "junit_runner.xml")
		}
	}
}

// TestParseFile_PropagatesParseError surfaces malformed XML from Parse.
func TestParseFile_PropagatesParseError(t *testing.T) {
	_, err := ParseFile([]byte("<not-valid-xml"), "junit_runner.xml")
	if err == nil {
		t.Fatal("expected error from malformed XML")
	}
}

func TestIdentity(t *testing.T) {
	xml := `<testsuite name="suite"><testcase name=" test " classname="class"/></testsuite>`
	cases, err := Parse([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Identity(cases[0]), "suite\x00class\x00test"; got != want {
		t.Fatalf("Identity = %q, want %q", got, want)
	}
	if cases[0].SuiteName != "suite" || cases[0].ClassName != "class" {
		t.Fatalf("case metadata = %+v", cases[0])
	}
}

func TestIdentityFallsBackToExactName(t *testing.T) {
	tc := models.TestCase{Name: " test "}
	if got, want := Identity(tc), "name\x00test"; got != want {
		t.Fatalf("Identity = %q, want %q", got, want)
	}
	if got := Identities(models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class"}); len(got) != 2 || got[1] != "name\x00test" {
		t.Fatalf("Identities = %q", got)
	}
}
