package webrtc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// A data channel carries protocol events as text messages, one event per
// message, and that is the whole of the framing for everything a voice session
// sends.
//
// Video does not fit in it. SCTP negotiates a maximum message size, each peer
// advertises its own, and the smallest in the field is Safari's 64 KiB - well
// under one screen frame, which is base64 and therefore a third larger again
// than the image. A peer that sends past the limit does not get a truncated
// message: pion refuses the write and the frame never leaves, so the failure
// looks like video silently not working.
//
// So a message the peer cannot take in one piece is carried as a sequence of
// binary chunk frames. Text means a protocol event; binary means a chunk of
// one. The two cannot be confused, a client that ignores binary messages
// behaves exactly as it does today, and the reassembled bytes are the same
// event the protocol would have carried in one message - the adapter is still
// a plain protocol client, because chunking is below the protocol rather than
// part of it.
const (
	chunkVersion     = 1
	chunkHeaderBytes = 14
	chunkFinalFlag   = 1 << 0

	// chunkPayloadBytes is what one chunk carries when the peer's limit
	// allows it. 16 KiB is comfortably under every implementation's cap and
	// large enough that a screen frame costs tens of chunks rather than
	// hundreds.
	chunkPayloadBytes = 16 << 10

	// conservativeMessageBytes is assumed when the association has not
	// reported a negotiated size yet. It is the smallest value any peer in
	// the field advertises, so assuming it can only be too cautious.
	conservativeMessageBytes = 64 << 10

	// maxReassemblyBytes bounds one message being reassembled, and
	// maxReassemblyMessages bounds how many are in flight at once. Both exist
	// because reassembly is memory a peer controls, and a peer that opens
	// messages it never finishes should cost a bounded amount.
	maxReassemblyBytes    = 8 << 20
	maxReassemblyMessages = 4
)

var chunkMagic = [4]byte{'O', 'R', 'T', 'C'}

// errNotAChunk marks a binary message that is not chunk framing at all, so a
// caller can tell "malformed chunk" from "not ours".
var errNotAChunk = errors.New("not a transport chunk frame")

// splitChunks divides one protocol event into chunk frames.
//
// The identifier is per message rather than global sequencing: chunks of two
// messages never interleave on an ordered channel, but the identifier means a
// reassembler does not have to depend on that to be correct.
func splitChunks(identifier uint32, message []byte, payloadBytes int) [][]byte {
	if payloadBytes <= 0 {
		payloadBytes = chunkPayloadBytes
	}
	count := (len(message) + payloadBytes - 1) / payloadBytes
	if count == 0 {
		count = 1
	}
	frames := make([][]byte, 0, count)
	for index := 0; index < count; index++ {
		start := index * payloadBytes
		end := min(start+payloadBytes, len(message))
		frame := make([]byte, chunkHeaderBytes+(end-start))
		copy(frame[0:4], chunkMagic[:])
		frame[4] = chunkVersion
		if index == count-1 {
			frame[5] = chunkFinalFlag
		}
		binary.BigEndian.PutUint32(frame[6:10], identifier)
		binary.BigEndian.PutUint16(frame[10:12], uint16(index))
		binary.BigEndian.PutUint16(frame[12:14], uint16(count))
		copy(frame[chunkHeaderBytes:], message[start:end])
		frames = append(frames, frame)
	}
	return frames
}

// chunkPayloadFor sizes a chunk against what the peer will accept.
//
// The negotiated maximum is what pion enforces on the write, so a chunk has to
// fit inside it with its own header. Zero means the association has not
// reported one yet, which is treated as the smallest value anyone advertises.
func chunkPayloadFor(negotiated uint32) int {
	limit := int(negotiated)
	if limit <= 0 {
		limit = conservativeMessageBytes
	}
	payload := limit - chunkHeaderBytes
	return min(payload, chunkPayloadBytes)
}

// wholeMessageFits reports whether a message can be sent as one text message.
//
// A margin below the negotiated size covers the framing SCTP adds around the
// payload, so the decision does not sit exactly on the limit it is testing.
func wholeMessageFits(size int, negotiated uint32) bool {
	limit := int(negotiated)
	if limit <= 0 {
		limit = conservativeMessageBytes
	}
	return size <= limit-chunkHeaderBytes
}

type partialMessage struct {
	count    int
	received int
	bytes    int
	chunks   [][]byte
}

// reassembler rebuilds chunked messages.
//
// It is deliberately strict. A frame that disagrees with what an identifier
// already established, or that would push a message past the bound, discards
// that message rather than trying to recover it: half an event is not a
// smaller event, it is a parse error further downstream.
type reassembler struct {
	mu      sync.Mutex
	partial map[uint32]*partialMessage
}

func newReassembler() *reassembler {
	return &reassembler{partial: make(map[uint32]*partialMessage)}
}

// accept takes one binary message and returns a complete protocol event, or
// nil if more chunks are still outstanding.
func (assembler *reassembler) accept(frame []byte) ([]byte, error) {
	if len(frame) < chunkHeaderBytes || [4]byte(frame[0:4]) != chunkMagic {
		return nil, errNotAChunk
	}
	if frame[4] != chunkVersion {
		return nil, fmt.Errorf("unsupported transport chunk version %d", frame[4])
	}
	identifier := binary.BigEndian.Uint32(frame[6:10])
	index := int(binary.BigEndian.Uint16(frame[10:12]))
	count := int(binary.BigEndian.Uint16(frame[12:14]))
	payload := frame[chunkHeaderBytes:]
	if count == 0 || index >= count {
		return nil, fmt.Errorf("transport chunk %d of %d is not addressable", index, count)
	}

	assembler.mu.Lock()
	defer assembler.mu.Unlock()
	message, exists := assembler.partial[identifier]
	if !exists {
		if len(assembler.partial) >= maxReassemblyMessages {
			return nil, fmt.Errorf("more than %d partial messages in flight", maxReassemblyMessages)
		}
		message = &partialMessage{count: count, chunks: make([][]byte, count)}
		assembler.partial[identifier] = message
	}
	if message.count != count {
		delete(assembler.partial, identifier)
		return nil, fmt.Errorf("transport chunk claims %d chunks, message has %d", count, message.count)
	}
	if message.chunks[index] != nil {
		delete(assembler.partial, identifier)
		return nil, fmt.Errorf("duplicate transport chunk %d", index)
	}
	if message.bytes+len(payload) > maxReassemblyBytes {
		delete(assembler.partial, identifier)
		return nil, fmt.Errorf("reassembled message exceeds %d bytes", maxReassemblyBytes)
	}
	message.chunks[index] = payload
	message.received++
	message.bytes += len(payload)
	if message.received != message.count {
		return nil, nil
	}
	delete(assembler.partial, identifier)
	complete := make([]byte, 0, message.bytes)
	for _, chunk := range message.chunks {
		complete = append(complete, chunk...)
	}
	return complete, nil
}
