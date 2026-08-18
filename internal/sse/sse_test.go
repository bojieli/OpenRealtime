package sse

import (
	"strings"
	"testing"
)

func TestReadJoinsDataAndIgnoresOtherFields(t *testing.T) {
	t.Parallel()
	var payloads []string
	err := Read(strings.NewReader("event: message\nid: 1\ndata: {\"a\":\ndata: 1}\n\n: heartbeat\n\ndata: [DONE]\n\n"), 0, func(data []byte) error {
		payloads = append(payloads, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 2 || payloads[0] != "{\"a\":\n1}" || payloads[1] != "[DONE]" {
		t.Fatalf("unexpected payloads: %#v", payloads)
	}
}

func TestReadFlushesFinalEventAtEOF(t *testing.T) {
	t.Parallel()
	var payload string
	if err := Read(strings.NewReader("data: final"), 1024, func(data []byte) error {
		payload = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if payload != "final" {
		t.Fatalf("got %q", payload)
	}
}
