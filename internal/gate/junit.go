package gate

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
)

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Cases     []junitCase `xml:"testcase"`
	SystemOut string      `xml:"system-out,omitempty"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// JUnit renders the verdict as JUnit XML so a CI system that already shows test
// results can show the gate's. Every case is one testcase; a case with a
// regression carries a failure whose type is the first regression's kind and
// whose body lists them all. Warnings are written to system-out and do not fail
// the testcase, matching how they do not fail the gate.
func (r Result) JUnit() ([]byte, error) {
	regressions := group(r.Regressions)
	warnings := group(r.Warnings)

	ids := map[string]bool{}
	for _, id := range r.cases {
		ids[id] = true
	}
	// A Result assembled by hand has no case list; its findings still name cases.
	for id := range regressions {
		ids[id] = true
	}
	for id := range warnings {
		ids[id] = true
	}
	delete(ids, "")
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)

	suite := junitSuite{Name: r.Candidate, SystemOut: lines(warnings[""])}
	for _, id := range names {
		tc := junitCase{Name: id, ClassName: r.Candidate, SystemOut: lines(warnings[id])}
		if found := regressions[id]; len(found) > 0 {
			tc.Failure = &junitFailure{
				Message: found[0].Detail,
				Type:    found[0].Kind,
				Body:    lines(found),
			}
			suite.Failures++
		}
		suite.Cases = append(suite.Cases, tc)
	}
	suite.Tests = len(suite.Cases)

	body, err := xml.MarshalIndent(junitSuites{Suites: []junitSuite{suite}}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}

func group(findings []Finding) map[string][]Finding {
	out := map[string][]Finding{}
	for _, f := range findings {
		out[f.Case] = append(out[f.Case], f)
	}
	return out
}

func lines(findings []Finding) string {
	var b strings.Builder
	for i, f := range findings {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s: %s", f.Kind, f.Detail)
	}
	return b.String()
}
