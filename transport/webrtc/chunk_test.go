package webrtc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestChunkingRoundTripsAMessageLargerThanOneSCTPMessage(t *testing.T) {
	t.Parallel()
	message := bytes.Repeat([]byte("a screen frame is mostly base64 "), 4096)
	assembler := newReassembler()
	frames := splitChunks(7, message, chunkPayloadBytes)
	if len(frames) < 2 {
		t.Fatalf("expected the message to be split, got %d frames", len(frames))
	}
	var complete []byte
	for index, frame := range frames {
		if len(frame) > chunkPayloadBytes+chunkHeaderBytes {
			t.Fatalf("frame %d is %d bytes, past the chunk size", index, len(frame))
		}
		result, err := assembler.accept(frame)
		if err != nil {
			t.Fatalf("accept frame %d: %v", index, err)
		}
		if index < len(frames)-1 && result != nil {
			t.Fatalf("frame %d completed a message early", index)
		}
		if index == len(frames)-1 {
			complete = result
		}
	}
	if !bytes.Equal(complete, message) {
		t.Fatalf("reassembled %d bytes, sent %d", len(complete), len(message))
	}
	if len(assembler.partial) != 0 {
		t.Fatalf("a completed message must not stay in flight: %d held", len(assembler.partial))
	}
}

func TestChunksOfDifferentMessagesDoNotInterleaveIntoOne(t *testing.T) {
	t.Parallel()
	first := bytes.Repeat([]byte("1"), chunkPayloadBytes*2)
	second := bytes.Repeat([]byte("2"), chunkPayloadBytes*2)
	assembler := newReassembler()
	firstFrames := splitChunks(1, first, chunkPayloadBytes)
	secondFrames := splitChunks(2, second, chunkPayloadBytes)

	// Interleaved on purpose: an ordered channel would not do this, and the
	// identifier is what makes not depending on that correct.
	for index := range firstFrames {
		if _, err := assembler.accept(firstFrames[index]); err != nil {
			t.Fatalf("first %d: %v", index, err)
		}
		result, err := assembler.accept(secondFrames[index])
		if err != nil {
			t.Fatalf("second %d: %v", index, err)
		}
		if index == len(secondFrames)-1 && !bytes.Equal(result, second) {
			t.Fatal("the second message did not reassemble to itself")
		}
	}
}

func TestATextSizedMessageIsStillOneChunk(t *testing.T) {
	t.Parallel()
	frames := splitChunks(1, []byte(`{"type":"response.done"}`), chunkPayloadBytes)
	if len(frames) != 1 {
		t.Fatalf("expected one frame, got %d", len(frames))
	}
	if frames[0][5]&chunkFinalFlag == 0 {
		t.Fatal("a single chunk must be final")
	}
}

func TestAnEmptyMessageStillProducesOneChunk(t *testing.T) {
	t.Parallel()
	assembler := newReassembler()
	frames := splitChunks(1, nil, chunkPayloadBytes)
	if len(frames) != 1 {
		t.Fatalf("expected one frame, got %d", len(frames))
	}
	result, err := assembler.accept(frames[0])
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if len(result) != 0 || result == nil {
		t.Fatalf("expected an empty message, got %v", result)
	}
}

func TestSomethingThatIsNotAChunkIsNotMistakenForOne(t *testing.T) {
	t.Parallel()
	assembler := newReassembler()
	for _, input := range [][]byte{
		[]byte(`{"type":"response.done"}`),
		{},
		{'O', 'R', 'T', 'C'},
	} {
		if _, err := assembler.accept(input); err != errNotAChunk {
			t.Fatalf("expected %v for %q, got %v", errNotAChunk, input, err)
		}
	}
}

func TestAChunkThatContradictsItsMessageDiscardsIt(t *testing.T) {
	t.Parallel()
	message := bytes.Repeat([]byte("x"), chunkPayloadBytes*3)
	for name, corrupt := range map[string]func(frames [][]byte) [][]byte{
		"a repeated index": func(frames [][]byte) [][]byte {
			return [][]byte{frames[0], frames[0]}
		},
		"a disagreeing count": func(frames [][]byte) [][]byte {
			second := bytes.Clone(frames[1])
			binary.BigEndian.PutUint16(second[12:14], 9)
			return [][]byte{frames[0], second}
		},
	} {
		t.Run(name, func(t *testing.T) {
			assembler := newReassembler()
			frames := corrupt(splitChunks(3, message, chunkPayloadBytes))
			var lastErr error
			for _, frame := range frames {
				if _, err := assembler.accept(frame); err != nil {
					lastErr = err
				}
			}
			if lastErr == nil {
				t.Fatal("expected the message to be refused")
			}
			if len(assembler.partial) != 0 {
				t.Fatal("a refused message must not stay held")
			}
		})
	}
}

func TestReassemblyIsBounded(t *testing.T) {
	t.Parallel()
	t.Run("by message count", func(t *testing.T) {
		assembler := newReassembler()
		message := bytes.Repeat([]byte("y"), chunkPayloadBytes*2)
		for identifier := 1; identifier <= maxReassemblyMessages; identifier++ {
			if _, err := assembler.accept(splitChunks(uint32(identifier), message, chunkPayloadBytes)[0]); err != nil {
				t.Fatalf("open %d: %v", identifier, err)
			}
		}
		_, err := assembler.accept(splitChunks(uint32(maxReassemblyMessages+1), message, chunkPayloadBytes)[0])
		if err == nil || !strings.Contains(err.Error(), "partial messages in flight") {
			t.Fatalf("expected the in-flight bound to hold, got %v", err)
		}
	})

	t.Run("by total size", func(t *testing.T) {
		assembler := newReassembler()
		// One chunk claiming a count that would reassemble past the bound.
		payload := bytes.Repeat([]byte("z"), chunkPayloadBytes)
		count := maxReassemblyBytes/chunkPayloadBytes + 2
		var err error
		for index := 0; index < count; index++ {
			frame := make([]byte, chunkHeaderBytes+len(payload))
			copy(frame[0:4], chunkMagic[:])
			frame[4] = chunkVersion
			binary.BigEndian.PutUint32(frame[6:10], 1)
			binary.BigEndian.PutUint16(frame[10:12], uint16(index))
			binary.BigEndian.PutUint16(frame[12:14], uint16(count))
			copy(frame[chunkHeaderBytes:], payload)
			if _, err = assembler.accept(frame); err != nil {
				break
			}
		}
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("expected the size bound to hold, got %v", err)
		}
	})
}

// The peer's negotiated size is what decides the shape of a send, so the two
// helpers that read it have to agree about what fits.
func TestSizingFollowsWhatThePeerNegotiated(t *testing.T) {
	t.Parallel()
	for _, negotiated := range []uint32{0, 1024, 65536, 262144, 1073741823} {
		payload := chunkPayloadFor(negotiated)
		if payload <= 0 {
			t.Fatalf("negotiated %d produced a chunk payload of %d", negotiated, payload)
		}
		if payload+chunkHeaderBytes > int(negotiated) && negotiated != 0 {
			t.Fatalf("a chunk of %d would not fit in %d", payload+chunkHeaderBytes, negotiated)
		}
		// Anything the peer can take whole must not be chunked, and anything
		// it cannot must be.
		if !wholeMessageFits(payload, negotiated) {
			t.Fatalf("a message of one chunk payload should fit in %d", negotiated)
		}
		if wholeMessageFits(int(negotiated)+1, negotiated) && negotiated != 0 {
			t.Fatalf("a message past %d must not be sent whole", negotiated)
		}
	}
}

func TestExportedEventCodecRoundTripsTextAndLargeEvents(t *testing.T) {
	t.Parallel()
	encoder := NewEventCodec()
	decoder := NewEventCodec()

	text := []byte(`{"type":"session.update"}`)
	frames := encoder.Encode(text, conservativeMessageBytes)
	if len(frames) != 1 || frames[0].Binary {
		t.Fatalf("small event framing = %+v", frames)
	}
	decoded, err := decoder.Decode(frames[0].Data, frames[0].Binary)
	if err != nil || !bytes.Equal(decoded, text) {
		t.Fatalf("small event decode = %q, %v", decoded, err)
	}

	large := bytes.Repeat([]byte("screen-pixels"), 20_000)
	frames = encoder.Encode(large, 32<<10)
	if len(frames) < 2 {
		t.Fatal("a large event must be chunked")
	}
	decoded = nil
	for index, frame := range frames {
		if !frame.Binary {
			t.Fatalf("large frame %d was sent as text", index)
		}
		decoded, err = decoder.Decode(frame.Data, frame.Binary)
		if err != nil {
			t.Fatalf("decode frame %d: %v", index, err)
		}
	}
	if !bytes.Equal(decoded, large) {
		t.Fatalf("large event decoded %d bytes, want %d", len(decoded), len(large))
	}
}
