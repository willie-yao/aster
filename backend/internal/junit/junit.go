// Package junit parses JUnit XML test result files into structured test cases.
package junit

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// xmlTestSuites mirrors the JUnit <testsuites> root.
type xmlTestSuites struct {
	XMLName    xml.Name       `xml:"testsuites"`
	TestSuites []xmlTestSuite `xml:"testsuite"`
}

type xmlTestSuite struct {
	XMLName   xml.Name      `xml:"testsuite"`
	Name      string        `xml:"name,attr"`
	Tests     int           `xml:"tests,attr"`
	Failures  int           `xml:"failures,attr"`
	Errors    int           `xml:"errors,attr"`
	Time      string        `xml:"time,attr"`
	TestCases []xmlTestCase `xml:"testcase"`
}

type xmlTestCase struct {
	Name      string      `xml:"name,attr"`
	ClassName string      `xml:"classname,attr"`
	Status    string      `xml:"status,attr"`
	Time      string      `xml:"time,attr"`
	Failure   *xmlFailure `xml:"failure"`
	Skipped   *xmlSkipped `xml:"skipped"`
}

type xmlFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type xmlSkipped struct {
	Message string `xml:"message,attr"`
}

// moduleLocationRe matches Go module paths with file:line references.
// Example: sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115.
var moduleLocationRe = regexp.MustCompile(
	`(sigs\.k8s\.io/[a-zA-Z0-9._-]+(?:/[a-zA-Z0-9._-]+)*)` + // module path
		`(?:@(v[0-9]+\.[0-9]+\.[0-9]+[a-zA-Z0-9._-]*))?` + // optional version
		`(/[a-zA-Z0-9._/-]+\.go):(\d+)`, // file path and line number
)

// Known module-to-GitHub-repo mappings.
var knownRepos = map[string]string{
	"sigs.k8s.io/cluster-api":                "kubernetes-sigs/cluster-api",
	"sigs.k8s.io/cluster-api-provider-azure": "kubernetes-sigs/cluster-api-provider-azure",
}

// Parse parses JUnit XML data and returns a slice of TestCase models. The
// resulting TestCases have JUnitFile unset; use ParseFile when the caller
// knows which file the data came from. Multi-JUnit builds rely on JUnitFile to
// disambiguate same-named cases across shards and suites.
func Parse(data []byte) ([]models.TestCase, error) {
	var suites xmlTestSuites
	if err := xml.Unmarshal(data, &suites); err == nil && len(suites.TestSuites) > 0 {
		return convertSuites(suites.TestSuites), nil
	}

	// Trust actual <testcase> elements rather than the self-reported tests count.
	// ClusterLoaderV2 can emit <testsuite tests="0"> while listing real cases.
	var suite xmlTestSuite
	if err := xml.Unmarshal(data, &suite); err == nil && len(suite.TestCases) > 0 {
		return convertSuites([]xmlTestSuite{suite}), nil
	}

	return nil, fmt.Errorf("junit: failed to parse XML as <testsuites> or <testsuite>")
}

// ParseFile parses JUnit XML data and stamps junitFile on every TestCase.
// junitFile should be the basename within the build's artifacts/ dir, not a
// full URL.
func ParseFile(data []byte, junitFile string) ([]models.TestCase, error) {
	cases, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for i := range cases {
		cases[i].JUnitFile = junitFile
	}
	return cases, nil
}

func convertSuites(suites []xmlTestSuite) []models.TestCase {
	var results []models.TestCase
	for _, suite := range suites {
		for _, tc := range suite.TestCases {
			results = append(results, convertTestCase(tc))
		}
	}
	return results
}

func convertTestCase(tc xmlTestCase) models.TestCase {
	m := models.TestCase{
		Name:            tc.Name,
		DurationSeconds: parseFloat(tc.Time),
	}

	switch {
	case tc.Failure != nil:
		m.Status = "failed"
		m.FailureMessage = tc.Failure.Message
		m.FailureBody = strings.TrimSpace(tc.Failure.Body)
		loc, url := ExtractFailureLocation(m.FailureBody)
		m.FailureLocation = loc
		m.FailureLocURL = url
	case tc.Skipped != nil || tc.Status == "skipped":
		m.Status = "skipped"
	default:
		m.Status = "passed"
	}

	return m
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// ExtractFailureLocation finds the first Go source file:line reference in a
// failure body and returns both the raw location string and a best-effort
// GitHub URL.
func ExtractFailureLocation(failureBody string) (location string, url string) {
	matches := moduleLocationRe.FindStringSubmatch(failureBody)
	if matches == nil {
		return "", ""
	}

	modulePath := matches[1] // example: sigs.k8s.io/cluster-api/test
	version := matches[2]    // example: v1.12.3 or ""
	filePath := matches[3]   // example: /framework/controlplane_helpers.go
	line := matches[4]       // example: 115

	location = modulePath
	if version != "" {
		location += "@" + version
	}
	location += filePath + ":" + line

	var ghRepo string
	var subPath string
	var bestLen int
	for prefix, repo := range knownRepos {
		if strings.HasPrefix(modulePath, prefix) && len(prefix) > bestLen {
			ghRepo = repo
			subPath = strings.TrimPrefix(modulePath, prefix)
			bestLen = len(prefix)
		}
	}
	if ghRepo == "" {
		return location, ""
	}

	ref := "main"
	if version != "" {
		ref = version
	}

	fullPath := subPath + filePath // example: /test/framework/controlplane_helpers.go
	fullPath = strings.TrimPrefix(fullPath, "/")

	url = fmt.Sprintf("https://github.com/%s/blob/%s/%s#L%s", ghRepo, ref, fullPath, line)
	return location, url
}
