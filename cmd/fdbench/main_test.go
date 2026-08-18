package main

import "testing"

func TestSplitCSVAndSafeName(t *testing.T) {
	values := splitCSV("alpha, beta,,gamma")
	if len(values) != 3 || values[1] != "beta" {
		t.Fatalf("splitCSV returned %#v", values)
	}
	if got := safeName("open realtime/one"); got != "open-realtime-one" {
		t.Fatalf("safeName returned %q", got)
	}
}
