package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsIncompleteInvocation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "requires -draft, -out") {
		t.Fatalf("incomplete invocation = %d, stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsNoncanonicalDraftBeforeCreatingOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-draft", "missing.json", "-out", "unused.json"},
		&stdout, &stderr); code != 2 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "draft invalid") {
		t.Fatalf("invalid draft = %d, stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
}
