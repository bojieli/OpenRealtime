package ffmpeg

import (
	"strings"
	"testing"
)

func TestResolveExecutableRejectsInvalidConfiguredTextBeforeLookup(t *testing.T) {
	for _, value := range []string{
		strings.Repeat("x", 4097), "\x00/usr/bin/ffmpeg", "\n/usr/bin/ffmpeg", "   ", string([]byte{0xff}),
	} {
		if _, err := resolveExecutable(value, "ffmpeg"); err == nil {
			t.Fatalf("resolveExecutable(%q) unexpectedly passed", value)
		}
	}
}

func TestParseVersionReturnsBoundedCanonicalIdentity(t *testing.T) {
	got, err := parseVersion([]byte("ffmpeg version 4.4.2-0ubuntu0.22.04.1 Copyright\n"), "ffmpeg")
	if err != nil || got != "ffmpeg-4.4.2-0ubuntu0.22.04.1" {
		t.Fatalf("parseVersion() = %q, %v", got, err)
	}
	for _, payload := range [][]byte{
		nil,
		[]byte("ffprobe version 4.4\n"),
		[]byte("ffmpeg version \n"),
		[]byte("ffmpeg version " + strings.Repeat("x", 256) + "\n"),
	} {
		if _, err := parseVersion(payload, "ffmpeg"); err == nil {
			t.Fatalf("parseVersion(%q) unexpectedly passed", payload)
		}
	}
}
