package scan

import "testing"

const grypeJSON = `{"matches":[
 {"vulnerability":{"id":"CVE-1","severity":"Low","fix":{"versions":[]}},"artifact":{"name":"a","version":"1"}},
 {"vulnerability":{"id":"CVE-2","severity":"Critical","fix":{"versions":["2.0"]}},"artifact":{"name":"b","version":"1"}},
 {"vulnerability":{"id":"CVE-3","severity":"High","fix":{"versions":[]}},"artifact":{"name":"c","version":"1"}},
 {"vulnerability":{"id":"GHSA-x","severity":"Unknown","fix":{"versions":[]}},"artifact":{"name":"d","version":"1"}}
]}`

func TestPolicy(t *testing.T) {
	r, err := ParseGrype([]byte(grypeJSON))
	if err != nil {
		t.Fatal(err)
	}
	if r.Findings[0].ID != "CVE-2" {
		t.Fatalf("findings not sorted by severity: %+v", r.Findings)
	}
	if n := len(r.AtLeast("high", nil)); n != 2 {
		t.Fatalf("high: got %d", n)
	}
	if n := len(r.AtLeast("high", []string{"CVE-2"})); n != 1 {
		t.Fatalf("ignore not applied: got %d", n)
	}
	if n := len(r.AtLeast("none", nil)); n != 0 {
		t.Fatalf("none must never fail: got %d", n)
	}
	if n := len(r.AtLeast("negligible", nil)); n != 3 {
		t.Fatalf("negligible: got %d", n)
	}
	if c := r.Counts(); c["critical"] != 1 || c["low"] != 1 {
		t.Fatalf("counts %v", c)
	}
}
