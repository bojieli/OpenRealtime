package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
)

var (
	ebmlID               = []byte{0x1a, 0x45, 0xdf, 0xa3}
	ebmlVersionID        = []byte{0x42, 0x86}
	ebmlReadVersionID    = []byte{0x42, 0xf7}
	ebmlMaxIDLengthID    = []byte{0x42, 0xf2}
	ebmlMaxSizeLengthID  = []byte{0x42, 0xf3}
	docTypeID            = []byte{0x42, 0x82}
	docTypeVersionID     = []byte{0x42, 0x87}
	docTypeReadVersionID = []byte{0x42, 0x85}
	segmentID            = []byte{0x18, 0x53, 0x80, 0x67}
	infoID               = []byte{0x15, 0x49, 0xa9, 0x66}
	timecodeScaleID      = []byte{0x2a, 0xd7, 0xb1}
	muxingAppID          = []byte{0x4d, 0x80}
	writingAppID         = []byte{0x57, 0x41}
	tracksID             = []byte{0x16, 0x54, 0xae, 0x6b}
	trackEntryID         = []byte{0xae}
	trackNumberID        = []byte{0xd7}
	trackUIDID           = []byte{0x73, 0xc5}
	trackTypeID          = []byte{0x83}
	flagLacingID         = []byte{0x9c}
	codecIDID            = []byte{0x86}
	videoID              = []byte{0xe0}
	pixelWidthID         = []byte{0xb0}
	pixelHeightID        = []byte{0xba}
	clusterID            = []byte{0x1f, 0x43, 0xb6, 0x75}
	timestampID          = []byte{0xe7}
	blockGroupID         = []byte{0xa0}
	blockID              = []byte{0xa1}
	blockDurationID      = []byte{0x9b}
)

// writeExactMatroska creates a minimal V_MJPEG Matroska stream with a one-
// microsecond timecode unit. One cluster per evidence frame avoids the signed
// 16-bit relative-block-time limit. A private repeated final frame at the
// declared end acts as a sentinel so the encoder preserves the last duration;
// it is removed before the published MP4 is attested.
func writeExactMatroska(
	ctx context.Context,
	outputPath string,
	inputRoot string,
	entries []timelineEntry,
	width, height int,
) (returnErr error) {
	if ctx == nil || len(entries) == 0 || width <= 0 || height <= 0 {
		return errors.New("exact Matroska request is invalid")
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return errors.New("create exact Matroska timeline")
	}
	success := false
	defer func() {
		if closeErr := file.Close(); returnErr == nil && closeErr != nil {
			returnErr = errors.New("close exact Matroska timeline")
		}
		if !success || returnErr != nil {
			_ = os.Remove(outputPath)
		}
	}()

	header := &bytes.Buffer{}
	writeUnsignedElement(header, ebmlVersionID, 1)
	writeUnsignedElement(header, ebmlReadVersionID, 1)
	writeUnsignedElement(header, ebmlMaxIDLengthID, 4)
	writeUnsignedElement(header, ebmlMaxSizeLengthID, 8)
	writeBytesElement(header, docTypeID, []byte("matroska"))
	writeUnsignedElement(header, docTypeVersionID, 4)
	writeUnsignedElement(header, docTypeReadVersionID, 2)
	if err := writeBytesElementTo(file, ebmlID, header.Bytes()); err != nil {
		return errors.New("write exact Matroska EBML header")
	}
	if _, err := file.Write(segmentID); err != nil {
		return errors.New("write exact Matroska segment")
	}
	// Eight-byte unknown length. The segment is terminated by EOF.
	if _, err := file.Write([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}); err != nil {
		return errors.New("write exact Matroska unknown segment size")
	}

	info := &bytes.Buffer{}
	writeUnsignedElement(info, timecodeScaleID, 1000) // nanoseconds: one microsecond
	writeBytesElement(info, muxingAppID, []byte("OpenRealtime"))
	writeBytesElement(info, writingAppID, []byte("OpenRealtime exact review timeline v1"))
	if err := writeBytesElementTo(file, infoID, info.Bytes()); err != nil {
		return errors.New("write exact Matroska info")
	}
	geometry := yuv420pGeometry(width, height)
	video := &bytes.Buffer{}
	writeUnsignedElement(video, pixelWidthID, uint64(geometry.encodedWidth))
	writeUnsignedElement(video, pixelHeightID, uint64(geometry.encodedHeight))
	track := &bytes.Buffer{}
	writeUnsignedElement(track, trackNumberID, 1)
	writeUnsignedElement(track, trackUIDID, 1)
	writeUnsignedElement(track, trackTypeID, 1)
	writeUnsignedElement(track, flagLacingID, 0)
	writeBytesElement(track, codecIDID, []byte("V_MJPEG"))
	writeBytesElement(track, videoID, video.Bytes())
	tracks := &bytes.Buffer{}
	writeBytesElement(tracks, trackEntryID, track.Bytes())
	if err := writeBytesElementTo(file, tracksID, tracks.Bytes()); err != nil {
		return errors.New("write exact Matroska video track")
	}

	muxEntries := append([]timelineEntry(nil), entries...)
	last := entries[len(entries)-1]
	if last.ptsUS > int64(^uint64(0)>>1)-last.durationUS {
		return errors.New("exact Matroska sentinel timestamp overflows")
	}
	muxEntries = append(muxEntries, timelineEntry{
		path: last.path, ptsUS: last.ptsUS + last.durationUS, durationUS: 1000,
	})
	var totalBytes int64
	for _, entry := range muxEntries {
		if err := ctx.Err(); err != nil {
			return err
		}
		framePath := filepath.Join(inputRoot, filepath.FromSlash(entry.path))
		encoded, err := normalizedJPEG(framePath, geometry)
		if err != nil {
			return err
		}
		if int64(len(encoded)) > maximumInputBytes-totalBytes {
			return errors.New("exact Matroska timeline exceeds its byte bound")
		}
		totalBytes += int64(len(encoded))
		blockPayloadBytes := 4 + len(encoded) // track VINT + relative time + flags + frame
		blockElementBytes := encodedElementSize(blockID, uint64(blockPayloadBytes))
		durationPayloadBytes := unsignedWidth(uint64(entry.durationUS))
		durationElementBytes := encodedElementSize(blockDurationID, uint64(durationPayloadBytes))
		groupPayloadBytes := blockElementBytes + durationElementBytes
		groupElementBytes := encodedElementSize(blockGroupID, uint64(groupPayloadBytes))
		timestampPayloadBytes := unsignedWidth(uint64(entry.ptsUS))
		timestampElementBytes := encodedElementSize(timestampID, uint64(timestampPayloadBytes))
		clusterPayloadBytes := timestampElementBytes + groupElementBytes
		if err := writeElementHeader(file, clusterID, uint64(clusterPayloadBytes)); err != nil {
			return errors.New("write exact Matroska cluster")
		}
		if err := writeUnsignedElementTo(file, timestampID, uint64(entry.ptsUS)); err != nil {
			return errors.New("write exact Matroska cluster timestamp")
		}
		if err := writeElementHeader(file, blockGroupID, uint64(groupPayloadBytes)); err != nil {
			return errors.New("write exact Matroska block group")
		}
		if err := writeElementHeader(file, blockID, uint64(blockPayloadBytes)); err != nil {
			return errors.New("write exact Matroska block")
		}
		if _, err := file.Write([]byte{0x81, 0x00, 0x00, 0x00}); err != nil {
			return errors.New("write exact Matroska block header")
		}
		if _, err := file.Write(encoded); err != nil {
			return errors.New("write exact Matroska frame")
		}
		if err := writeUnsignedElementTo(file, blockDurationID, uint64(entry.durationUS)); err != nil {
			return errors.New("write exact Matroska frame duration")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync exact Matroska timeline")
	}
	success = true
	return nil
}

func normalizedJPEG(path string, geometry videoGeometry) ([]byte, error) {
	file, info, err := openStableRegular(path, maximumInputBytes)
	if err != nil {
		return nil, errors.New("open exact Matroska frame")
	}
	defer file.Close()
	configuration, _, err := image.DecodeConfig(io.LimitReader(file, maximumInputBytes+1))
	if err != nil || configuration.Width != geometry.sourceWidth ||
		configuration.Height != geometry.sourceHeight {
		return nil, errors.New("exact Matroska frame dimensions do not match the declaration")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("rewind exact Matroska frame")
	}
	decoded, _, err := image.Decode(io.LimitReader(file, maximumInputBytes+1))
	if err != nil || decoded.Bounds().Dx() != geometry.sourceWidth ||
		decoded.Bounds().Dy() != geometry.sourceHeight {
		return nil, errors.New("decode exact Matroska frame")
	}
	if err := verifyOpenIdentity(path, file, info); err != nil {
		return nil, err
	}
	// Keep every captured source pixel at its original coordinate. yuv420p
	// requires even dimensions, so only the private derived timeline gets an
	// opaque-black right column and/or bottom row. The raw evidence file is
	// never rewritten or cropped.
	padded := image.NewRGBA(image.Rect(0, 0, geometry.encodedWidth, geometry.encodedHeight))
	draw.Draw(padded, padded.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	draw.Draw(padded, image.Rect(0, 0, geometry.sourceWidth, geometry.sourceHeight),
		decoded, decoded.Bounds().Min, draw.Src)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, padded, &jpeg.Options{Quality: 95}); err != nil ||
		int64(encoded.Len()) > maximumInputBytes {
		return nil, errors.New("normalize exact Matroska frame")
	}
	return encoded.Bytes(), nil
}

func writeBytesElement(buffer *bytes.Buffer, id, payload []byte) {
	_ = writeElementHeader(buffer, id, uint64(len(payload)))
	_, _ = buffer.Write(payload)
}

func writeUnsignedElement(buffer *bytes.Buffer, id []byte, value uint64) {
	width := unsignedWidth(value)
	_ = writeElementHeader(buffer, id, uint64(width))
	writeUnsigned(buffer, value, width)
}

func writeBytesElementTo(writer io.Writer, id, payload []byte) error {
	if err := writeElementHeader(writer, id, uint64(len(payload))); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}

func writeUnsignedElementTo(writer io.Writer, id []byte, value uint64) error {
	width := unsignedWidth(value)
	if err := writeElementHeader(writer, id, uint64(width)); err != nil {
		return err
	}
	return writeUnsigned(writer, value, width)
}

func writeElementHeader(writer io.Writer, id []byte, payloadBytes uint64) error {
	if _, err := writer.Write(id); err != nil {
		return err
	}
	_, err := writer.Write(encodeElementSize(payloadBytes))
	return err
}

func encodedElementSize(id []byte, payloadBytes uint64) int {
	return len(id) + len(encodeElementSize(payloadBytes)) + int(payloadBytes)
}

func encodeElementSize(value uint64) []byte {
	for width := 1; width <= 8; width++ {
		maximum := uint64(1)<<(7*width) - 2
		if width == 8 {
			maximum = uint64(1)<<56 - 2
		}
		if value <= maximum {
			encoded := make([]byte, width)
			remaining := value
			for index := width - 1; index >= 0; index-- {
				encoded[index] = byte(remaining)
				remaining >>= 8
			}
			encoded[0] |= byte(1 << (8 - width))
			return encoded
		}
	}
	panic("EBML element exceeds 56-bit size")
}

func unsignedWidth(value uint64) int {
	width := 1
	for value > 0xff {
		value >>= 8
		width++
	}
	return width
}

func writeUnsigned(writer io.Writer, value uint64, width int) error {
	encoded := make([]byte, width)
	for index := width - 1; index >= 0; index-- {
		encoded[index] = byte(value)
		value >>= 8
	}
	_, err := writer.Write(encoded)
	return err
}
