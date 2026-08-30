package ffmpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

func TestPatchMP4BytesProducesExactMovieAndMediaTimelines(t *testing.T) {
	request := exactPatchRequest()
	payload := classicMP4Fixture(t, request, false)
	if err := patchMP4Bytes(payload, request, 499_211); err != nil {
		t.Fatal(err)
	}

	top, err := parseMP4Boxes(payload, 0, len(payload), true)
	if err != nil {
		t.Fatal(err)
	}
	moov, err := oneMP4Box(top, "moov")
	if err != nil {
		t.Fatal(err)
	}
	children, err := parseMP4Boxes(payload, moov.payloadStart, moov.end, false)
	if err != nil {
		t.Fatal(err)
	}
	mvhd, err := oneMP4Box(children, "mvhd")
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(payload[mvhd.payloadStart+12 : mvhd.payloadStart+16]); got != 1_000_000 {
		t.Fatalf("movie time scale = %d", got)
	}
	if got := binary.BigEndian.Uint32(payload[mvhd.payloadStart+16 : mvhd.payloadStart+20]); got != uint32(request.AttemptEndUS) {
		t.Fatalf("movie duration = %d", got)
	}

	seenVideo, seenAudio := false, false
	for _, track := range children {
		if track.typeCode != "trak" {
			continue
		}
		handler, tkhd, elst, err := trackTimelineBoxes(payload, track)
		if err != nil {
			t.Fatal(err)
		}
		if got := binary.BigEndian.Uint32(payload[tkhd.payloadStart+20 : tkhd.payloadStart+24]); got != uint32(request.AttemptEndUS) {
			t.Fatalf("%s track duration = %d", handler, got)
		}
		entries, count, err := editListEntries(payload, elst)
		if err != nil {
			t.Fatal(err)
		}
		switch handler {
		case "vide":
			seenVideo = true
			if count != 2 || binary.BigEndian.Uint32(payload[entries:entries+4]) != uint32(request.FirstFrameUS) ||
				binary.BigEndian.Uint32(payload[entries+12:entries+16]) != uint32(request.AttemptEndUS-request.FirstFrameUS) {
				t.Fatal("video edit list does not exactly represent the presentation interval")
			}
			mdia := mustOneChild(t, payload, track, "mdia")
			mdhd := mustOneChild(t, payload, mdia, "mdhd")
			if got := binary.BigEndian.Uint32(payload[mdhd.payloadStart+16 : mdhd.payloadStart+20]); got != uint32(request.AttemptEndUS-request.FirstFrameUS) {
				t.Fatalf("video media duration = %d", got)
			}
			minf := mustOneChild(t, payload, mdia, "minf")
			stbl := mustOneChild(t, payload, minf, "stbl")
			stts := mustOneChild(t, payload, stbl, "stts")
			if got := binary.BigEndian.Uint32(payload[stts.end-4 : stts.end]); got != 499_211 {
				t.Fatalf("last video sample duration = %d", got)
			}
		case "soun":
			seenAudio = true
			if count != 1 || binary.BigEndian.Uint32(payload[entries:entries+4]) != uint32(request.AttemptEndUS) {
				t.Fatal("audio edit list does not exactly represent the attempt")
			}
		default:
			t.Fatalf("unexpected handler %q", handler)
		}
	}
	if !seenVideo || !seenAudio {
		t.Fatal("fixture lost a declared track")
	}
}

func TestPatchMP4BytesRejectsMalformedOrAmbiguousStructures(t *testing.T) {
	request := exactPatchRequest()
	valid := classicMP4Fixture(t, request, false)
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"truncated header", func(payload []byte) []byte { return payload[:7] }},
		{"trailing partial header", func(payload []byte) []byte { return append(payload, 0) }},
		{"duplicate ftyp", func(payload []byte) []byte { return append(payload, mp4TestBox("ftyp", nil)...) }},
		{"fragment", func(payload []byte) []byte { return append(payload, mp4TestBox("moof", nil)...) }},
		{"missing mdat", func(payload []byte) []byte {
			boxes, err := parseMP4Boxes(payload, 0, len(payload), true)
			if err != nil {
				t.Fatal(err)
			}
			return append([]byte(nil), payload[:boxes[len(boxes)-1].start]...)
		}},
		{"nested zero size", func(payload []byte) []byte {
			setBoxSize(t, payload, "mdia", 0)
			return payload
		}},
		{"unsupported handler", func(payload []byte) []byte {
			index := bytes.Index(payload, []byte("vide"))
			if index < 0 {
				t.Fatal("missing video handler")
			}
			copy(payload[index:index+4], "meta")
			return payload
		}},
		{"edit list version", func(payload []byte) []byte {
			index := boxPayloadOffset(t, payload, "elst", 0)
			payload[index] = 1
			return payload
		}},
		{"edit list media rate", func(payload []byte) []byte {
			index := boxPayloadOffset(t, payload, "elst", 0)
			binary.BigEndian.PutUint32(payload[index+16:index+20], 0)
			return payload
		}},
		{"edit list media time", func(payload []byte) []byte {
			index := boxPayloadOffset(t, payload, "elst", 0)
			binary.BigEndian.PutUint32(payload[index+24:index+28], 1)
			return payload
		}},
		{"temporary movie time scale", func(payload []byte) []byte {
			index := boxPayloadOffset(t, payload, "mvhd", 0)
			binary.BigEndian.PutUint32(payload[index+12:index+16], 90_000)
			return payload
		}},
		{"video time scale", func(payload []byte) []byte {
			index := boxPayloadOffset(t, payload, "mdhd", 0)
			binary.BigEndian.PutUint32(payload[index+12:index+16], 90_000)
			return payload
		}},
		{"sample count", func(payload []byte) []byte {
			index := boxPayloadOffset(t, payload, "stts", 0)
			binary.BigEndian.PutUint32(payload[index+8:index+12], 2)
			return payload
		}},
		{"sample table entry count", func(payload []byte) []byte {
			index := boxPayloadOffset(t, payload, "stts", 0)
			binary.BigEndian.PutUint32(payload[index+4:index+8], 4)
			return payload
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := test.mutate(append([]byte(nil), valid...))
			if err := patchMP4Bytes(payload, request, 499_211); err == nil {
				t.Fatal("malformed MP4 unexpectedly passed the exact patch boundary")
			}
		})
	}
}

func TestPatchMP4BytesSupportsZeroPresentationOffset(t *testing.T) {
	request := exactPatchRequest()
	request.FirstFrameUS = 0
	payload := classicMP4Fixture(t, request, true)
	if err := patchMP4Bytes(payload, request, 499_211); err != nil {
		t.Fatal(err)
	}
	index := boxPayloadOffset(t, payload, "elst", 0)
	if count := binary.BigEndian.Uint32(payload[index+4 : index+8]); count != 1 {
		t.Fatalf("zero-offset edit count = %d", count)
	}
	if duration := binary.BigEndian.Uint32(payload[index+8 : index+12]); duration != uint32(request.AttemptEndUS) {
		t.Fatalf("zero-offset edit duration = %d", duration)
	}
}

func TestPatchClassicMP4TimelineHonorsCancellationWithoutMutation(t *testing.T) {
	request := exactPatchRequest()
	payload := classicMP4Fixture(t, request, false)
	path := filepath.Join(t.TempDir(), "output.mp4")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := patchClassicMP4Timeline(ctx, path, request, 499_211); !errors.Is(err, context.Canceled) {
		t.Fatalf("patch cancelled = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, payload) {
		t.Fatal("cancelled patch mutated the MP4")
	}
}

func BenchmarkPatchMP4Bytes(b *testing.B) {
	request := exactPatchRequest()
	fixture := classicMP4Fixture(b, request, false)
	b.ReportAllocs()
	b.SetBytes(int64(len(fixture)))
	for b.Loop() {
		payload := append([]byte(nil), fixture...)
		if err := patchMP4Bytes(payload, request, 499_211); err != nil {
			b.Fatal(err)
		}
	}
}

func exactPatchRequest() reviewmedia.EncodeRequest {
	return reviewmedia.EncodeRequest{
		FirstFrameUS: 125_123, AttemptEndUS: 1_250_000, ExpectedFrames: 3,
	}
}

func classicMP4Fixture(t testing.TB, request reviewmedia.EncodeRequest, zeroOffset bool) []byte {
	t.Helper()
	if zeroOffset != (request.FirstFrameUS == 0) {
		t.Fatal("fixture offset mode disagrees with request")
	}
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1_000)
	binary.BigEndian.PutUint32(mvhd[16:20], 1_250)
	video := mp4TrackFixture("vide", request, zeroOffset)
	audio := mp4TrackFixture("soun", request, false)
	moov := mp4TestBox("moov", append(append(mp4TestBox("mvhd", mvhd), video...), audio...))
	return append(append(mp4TestBox("ftyp", []byte("isom")), moov...), mp4TestBox("mdat", []byte{1, 2, 3, 4})...)
}

func mp4TrackFixture(handler string, request reviewmedia.EncodeRequest, zeroOffset bool) []byte {
	tkhd := make([]byte, 24)
	binary.BigEndian.PutUint32(tkhd[20:24], 1_250)
	hdlr := make([]byte, 12)
	copy(hdlr[8:12], handler)
	mdhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhd[12:16], 1_000_000)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(request.AttemptEndUS-request.FirstFrameUS))
	var minf []byte
	if handler == "vide" {
		firstDuration := uint32(300_333)
		secondDuration := uint32(request.AttemptEndUS - request.FirstFrameUS - int64(firstDuration) - 499_211)
		stts := make([]byte, 8+3*8)
		binary.BigEndian.PutUint32(stts[4:8], 3)
		for index, duration := range []uint32{firstDuration, secondDuration, 374_211} {
			offset := 8 + index*8
			binary.BigEndian.PutUint32(stts[offset:offset+4], 1)
			binary.BigEndian.PutUint32(stts[offset+4:offset+8], duration)
		}
		minf = mp4TestBox("minf", mp4TestBox("stbl", mp4TestBox("stts", stts)))
	}
	mdia := mp4TestBox("mdia", append(append(mp4TestBox("hdlr", hdlr), mp4TestBox("mdhd", mdhd)...), minf...))
	elst := make([]byte, 8)
	if handler == "vide" && !zeroOffset {
		binary.BigEndian.PutUint32(elst[4:8], 2)
		elst = append(elst, mp4EditEntry(125, -1)...)
		elst = append(elst, mp4EditEntry(1_125, 0)...)
	} else {
		binary.BigEndian.PutUint32(elst[4:8], 1)
		elst = append(elst, mp4EditEntry(1_250, 0)...)
	}
	edts := mp4TestBox("edts", mp4TestBox("elst", elst))
	return mp4TestBox("trak", append(append(mp4TestBox("tkhd", tkhd), mdia...), edts...))
}

func mp4EditEntry(duration uint32, mediaTime int32) []byte {
	result := make([]byte, 12)
	binary.BigEndian.PutUint32(result[0:4], duration)
	binary.BigEndian.PutUint32(result[4:8], uint32(mediaTime))
	binary.BigEndian.PutUint32(result[8:12], 0x00010000)
	return result
}

func mp4TestBox(kind string, payload []byte) []byte {
	result := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(result[0:4], uint32(len(result)))
	copy(result[4:8], kind)
	copy(result[8:], payload)
	return result
}

func mustOneChild(t testing.TB, payload []byte, parent mp4Box, kind string) mp4Box {
	t.Helper()
	children, err := parseMP4Boxes(payload, parent.payloadStart, parent.end, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := oneMP4Box(children, kind)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func boxPayloadOffset(t testing.TB, payload []byte, kind string, occurrence int) int {
	t.Helper()
	needle := []byte(kind)
	start := 0
	for index := 0; ; index++ {
		relative := bytes.Index(payload[start:], needle)
		if relative < 0 {
			t.Fatalf("missing box %q occurrence %d", kind, occurrence)
		}
		absolute := start + relative
		if index == occurrence {
			return absolute + 4
		}
		start = absolute + len(needle)
	}
}

func setBoxSize(t testing.TB, payload []byte, kind string, size uint32) {
	t.Helper()
	payloadOffset := boxPayloadOffset(t, payload, kind, 0)
	binary.BigEndian.PutUint32(payload[payloadOffset-8:payloadOffset-4], size)
}
