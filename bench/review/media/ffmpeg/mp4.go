package ffmpeg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

const maximumMP4Boxes = 100_000

type mp4Box struct {
	typeCode     string
	start        int
	payloadStart int
	end          int
}

// patchClassicMP4Timeline upgrades ffmpeg 4.4's fixed 1 kHz movie clock to a
// 1 MHz clock and rewrites only movie-clock duration/edit fields. Media sample
// bytes, media time scales, AAC priming media_time, and H.264 tables are left
// unchanged. This is needed because FFmpeg 4.4 exposes video_track_timescale
// but not the later movie_timescale muxer option.
func patchClassicMP4Timeline(
	ctx context.Context, path string, request reviewmedia.EncodeRequest, lastFrameDurationUS int64,
) (returnErr error) {
	if ctx == nil {
		return errors.New("patch MP4 timeline: nil context")
	}
	if request.AttemptEndUS <= request.FirstFrameUS || request.AttemptEndUS > math.MaxUint32 ||
		lastFrameDurationUS < 1000 || lastFrameDurationUS > math.MaxUint32 {
		return errors.New("patch MP4 timeline: duration exceeds the bounded version-zero edit list")
	}
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximumOutputBytes {
		return errors.New("patch MP4 timeline: output is not a bounded regular file")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return errors.New("open MP4 timeline for exact patch")
	}
	defer func() {
		if closeErr := file.Close(); returnErr == nil && closeErr != nil {
			returnErr = errors.New("close exact MP4 timeline")
		}
	}()
	opened, statErr := file.Stat()
	visible, visibleErr := os.Lstat(path)
	if statErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(before, opened) ||
		!os.SameFile(opened, visible) || opened.Size() != before.Size() {
		return errors.New("MP4 timeline identity changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, maximumOutputBytes+1))
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	if err != nil || int64(len(payload)) != before.Size() {
		return errors.New("read bounded MP4 timeline")
	}
	if err := patchMP4Bytes(payload, request, lastFrameDurationUS); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return errors.New("rewind exact MP4 timeline")
	}
	written, err := file.Write(payload)
	if err != nil || written != len(payload) || file.Sync() != nil {
		return errors.New("write exact MP4 timeline")
	}
	after, afterErr := file.Stat()
	visible, visibleErr = os.Lstat(path)
	if afterErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(opened, after) ||
		!os.SameFile(after, visible) || after.Size() != before.Size() {
		return errors.New("MP4 timeline identity changed while patching")
	}
	return ctx.Err()
}

func patchMP4Bytes(payload []byte, request reviewmedia.EncodeRequest, lastFrameDurationUS int64) error {
	top, err := parseMP4Boxes(payload, 0, len(payload), true)
	if err != nil {
		return err
	}
	moov, moovCount, ftypCount, mdatCount := mp4Box{}, 0, 0, 0
	for _, box := range top {
		switch box.typeCode {
		case "ftyp":
			ftypCount++
		case "moov":
			moov, moovCount = box, moovCount+1
		case "mdat":
			mdatCount++
		case "moof":
			return errors.New("exact MP4 timeline patch rejects fragmented input")
		}
	}
	if ftypCount != 1 || moovCount != 1 || mdatCount != 1 {
		return errors.New("exact MP4 timeline requires one ftyp, moov, and mdat")
	}
	children, err := parseMP4Boxes(payload, moov.payloadStart, moov.end, false)
	if err != nil {
		return err
	}
	mvhd, err := oneMP4Box(children, "mvhd")
	if err != nil {
		return err
	}
	if err := patchMovieHeader(payload, mvhd, uint64(request.AttemptEndUS)); err != nil {
		return err
	}
	videoTracks, audioTracks := 0, 0
	for _, child := range children {
		if child.typeCode != "trak" {
			continue
		}
		handler, tkhd, elst, err := trackTimelineBoxes(payload, child)
		if err != nil {
			return err
		}
		switch handler {
		case "vide":
			videoTracks++
			if err := patchTrackHeader(payload, tkhd, uint64(request.AttemptEndUS)); err != nil {
				return err
			}
			if err := patchVideoEditList(payload, elst, request.FirstFrameUS, request.AttemptEndUS); err != nil {
				return err
			}
			if err := patchVideoSampleDuration(payload, child, request.ExpectedFrames,
				lastFrameDurationUS, request.AttemptEndUS-request.FirstFrameUS); err != nil {
				return err
			}
		case "soun":
			audioTracks++
			if err := patchTrackHeader(payload, tkhd, uint64(request.AttemptEndUS)); err != nil {
				return err
			}
			if err := patchAudioEditList(payload, elst, request.AttemptEndUS); err != nil {
				return err
			}
		default:
			return fmt.Errorf("exact MP4 timeline contains unsupported %q track", handler)
		}
	}
	if videoTracks != 1 || audioTracks != 1 {
		return errors.New("exact MP4 timeline requires exactly one video and audio track")
	}
	return nil
}

func parseMP4Boxes(payload []byte, start, end int, allowTerminalSizeZero bool) ([]mp4Box, error) {
	if start < 0 || end < start || end > len(payload) {
		return nil, errors.New("MP4 box range is invalid")
	}
	result := make([]mp4Box, 0, 16)
	for cursor := start; cursor < end; {
		if len(result) >= maximumMP4Boxes || end-cursor < 8 {
			return nil, errors.New("MP4 box count or header is invalid")
		}
		size32 := binary.BigEndian.Uint32(payload[cursor : cursor+4])
		headerBytes, size := 8, uint64(size32)
		if size32 == 1 {
			if end-cursor < 16 {
				return nil, errors.New("MP4 extended box header is truncated")
			}
			headerBytes, size = 16, binary.BigEndian.Uint64(payload[cursor+8:cursor+16])
		} else if size32 == 0 {
			if !allowTerminalSizeZero {
				return nil, errors.New("nested MP4 box has an unbounded size")
			}
			size = uint64(end - cursor)
		}
		if size < uint64(headerBytes) || size > uint64(end-cursor) {
			return nil, errors.New("MP4 box size exceeds its parent")
		}
		boxEnd := cursor + int(size)
		if size32 == 0 && boxEnd != end {
			return nil, errors.New("zero-sized MP4 box is not terminal")
		}
		result = append(result, mp4Box{
			typeCode: string(payload[cursor+4 : cursor+8]), start: cursor,
			payloadStart: cursor + headerBytes, end: boxEnd,
		})
		cursor = boxEnd
	}
	return result, nil
}

func oneMP4Box(boxes []mp4Box, typeCode string) (mp4Box, error) {
	var result mp4Box
	count := 0
	for _, box := range boxes {
		if box.typeCode == typeCode {
			result, count = box, count+1
		}
	}
	if count != 1 {
		return mp4Box{}, fmt.Errorf("exact MP4 timeline requires one %s box", typeCode)
	}
	return result, nil
}

func trackTimelineBoxes(payload []byte, track mp4Box) (string, mp4Box, mp4Box, error) {
	children, err := parseMP4Boxes(payload, track.payloadStart, track.end, false)
	if err != nil {
		return "", mp4Box{}, mp4Box{}, err
	}
	tkhd, err := oneMP4Box(children, "tkhd")
	if err != nil {
		return "", mp4Box{}, mp4Box{}, err
	}
	mdia, err := oneMP4Box(children, "mdia")
	if err != nil {
		return "", mp4Box{}, mp4Box{}, err
	}
	edts, err := oneMP4Box(children, "edts")
	if err != nil {
		return "", mp4Box{}, mp4Box{}, err
	}
	mediaChildren, err := parseMP4Boxes(payload, mdia.payloadStart, mdia.end, false)
	if err != nil {
		return "", mp4Box{}, mp4Box{}, err
	}
	hdlr, err := oneMP4Box(mediaChildren, "hdlr")
	if err != nil || hdlr.end-hdlr.payloadStart < 12 {
		return "", mp4Box{}, mp4Box{}, errors.New("exact MP4 track handler is invalid")
	}
	handler := string(payload[hdlr.payloadStart+8 : hdlr.payloadStart+12])
	editChildren, err := parseMP4Boxes(payload, edts.payloadStart, edts.end, false)
	if err != nil {
		return "", mp4Box{}, mp4Box{}, err
	}
	elst, err := oneMP4Box(editChildren, "elst")
	return handler, tkhd, elst, err
}

func patchMovieHeader(payload []byte, box mp4Box, duration uint64) error {
	if box.end-box.payloadStart < 20 {
		return errors.New("MP4 movie header is truncated")
	}
	version := payload[box.payloadStart]
	switch version {
	case 0:
		if binary.BigEndian.Uint32(payload[box.payloadStart+12:box.payloadStart+16]) != 1_000 {
			return errors.New("MP4 movie header does not use the expected temporary time scale")
		}
		binary.BigEndian.PutUint32(payload[box.payloadStart+12:box.payloadStart+16], 1_000_000)
		binary.BigEndian.PutUint32(payload[box.payloadStart+16:box.payloadStart+20], uint32(duration))
	case 1:
		if box.end-box.payloadStart < 32 {
			return errors.New("version-one MP4 movie header is truncated")
		}
		if binary.BigEndian.Uint32(payload[box.payloadStart+20:box.payloadStart+24]) != 1_000 {
			return errors.New("MP4 movie header does not use the expected temporary time scale")
		}
		binary.BigEndian.PutUint32(payload[box.payloadStart+20:box.payloadStart+24], 1_000_000)
		binary.BigEndian.PutUint64(payload[box.payloadStart+24:box.payloadStart+32], duration)
	default:
		return errors.New("MP4 movie header version is unsupported")
	}
	return nil
}

func patchTrackHeader(payload []byte, box mp4Box, duration uint64) error {
	if box.end-box.payloadStart < 24 {
		return errors.New("MP4 track header is truncated")
	}
	switch payload[box.payloadStart] {
	case 0:
		binary.BigEndian.PutUint32(payload[box.payloadStart+20:box.payloadStart+24], uint32(duration))
	case 1:
		if box.end-box.payloadStart < 36 {
			return errors.New("version-one MP4 track header is truncated")
		}
		binary.BigEndian.PutUint64(payload[box.payloadStart+28:box.payloadStart+36], duration)
	default:
		return errors.New("MP4 track header version is unsupported")
	}
	return nil
}

func editListEntries(payload []byte, box mp4Box) (int, int, error) {
	if box.end-box.payloadStart < 8 || payload[box.payloadStart] != 0 {
		return 0, 0, errors.New("exact MP4 timeline requires a version-zero edit list")
	}
	count := int(binary.BigEndian.Uint32(payload[box.payloadStart+4 : box.payloadStart+8]))
	entriesStart := box.payloadStart + 8
	if count < 1 || count > 2 || entriesStart+count*12 != box.end {
		return 0, 0, errors.New("MP4 edit list has an unexpected shape")
	}
	for index := 0; index < count; index++ {
		offset := entriesStart + index*12
		if binary.BigEndian.Uint32(payload[offset+8:offset+12]) != 0x00010000 {
			return 0, 0, errors.New("MP4 edit list has a non-unit media rate")
		}
	}
	return entriesStart, count, nil
}

func patchVideoSampleDuration(
	payload []byte, track mp4Box, expectedFrames int, lastFrameDurationUS, mediaDurationUS int64,
) error {
	trackChildren, err := parseMP4Boxes(payload, track.payloadStart, track.end, false)
	if err != nil {
		return err
	}
	mdia, err := oneMP4Box(trackChildren, "mdia")
	if err != nil {
		return err
	}
	mediaChildren, err := parseMP4Boxes(payload, mdia.payloadStart, mdia.end, false)
	if err != nil {
		return err
	}
	mdhd, err := oneMP4Box(mediaChildren, "mdhd")
	if err != nil {
		return err
	}
	mediaTimescale, err := mediaHeaderTimescale(payload, mdhd)
	if err != nil || mediaTimescale != 1_000_000 {
		return errors.New("exact MP4 video media time scale is not one microsecond")
	}
	if err := patchMediaHeaderDuration(payload, mdhd, uint64(mediaDurationUS)); err != nil {
		return err
	}
	minf, err := oneMP4Box(mediaChildren, "minf")
	if err != nil {
		return err
	}
	mediaInfoChildren, err := parseMP4Boxes(payload, minf.payloadStart, minf.end, false)
	if err != nil {
		return err
	}
	stbl, err := oneMP4Box(mediaInfoChildren, "stbl")
	if err != nil {
		return err
	}
	sampleTableChildren, err := parseMP4Boxes(payload, stbl.payloadStart, stbl.end, false)
	if err != nil {
		return err
	}
	stts, err := oneMP4Box(sampleTableChildren, "stts")
	if err != nil {
		return err
	}
	if stts.end-stts.payloadStart < 16 || payload[stts.payloadStart] != 0 {
		return errors.New("exact MP4 video decoding-time table is invalid")
	}
	entryCount := int(binary.BigEndian.Uint32(payload[stts.payloadStart+4 : stts.payloadStart+8]))
	entriesStart := stts.payloadStart + 8
	if entryCount < 1 || entryCount > expectedFrames || entriesStart+entryCount*8 != stts.end {
		return errors.New("exact MP4 video decoding-time entries have an invalid shape")
	}
	totalSamples := uint64(0)
	totalDuration := uint64(0)
	for index := 0; index < entryCount; index++ {
		offset := entriesStart + index*8
		count := binary.BigEndian.Uint32(payload[offset : offset+4])
		delta := binary.BigEndian.Uint32(payload[offset+4 : offset+8])
		if count == 0 || delta == 0 {
			return errors.New("exact MP4 video decoding-time entry is empty")
		}
		totalSamples += uint64(count)
		if index == entryCount-1 {
			if count != 1 && delta != uint32(lastFrameDurationUS) {
				return errors.New("exact MP4 video last duration cannot be patched without resizing its sample table")
			}
			delta = uint32(lastFrameDurationUS)
		}
		entryDuration := uint64(count) * uint64(delta)
		if totalDuration > math.MaxUint64-entryDuration {
			return errors.New("exact MP4 video decoding-time duration overflows")
		}
		totalDuration += entryDuration
	}
	if totalSamples != uint64(expectedFrames) {
		return errors.New("exact MP4 video decoding-time sample count does not match evidence")
	}
	if totalDuration != uint64(mediaDurationUS) {
		return errors.New("exact MP4 video decoding-time duration does not match the presentation interval")
	}
	last := entriesStart + (entryCount-1)*8
	binary.BigEndian.PutUint32(payload[last+4:last+8], uint32(lastFrameDurationUS))
	return nil
}

func mediaHeaderTimescale(payload []byte, box mp4Box) (uint32, error) {
	if box.end-box.payloadStart < 20 {
		return 0, errors.New("MP4 media header is truncated")
	}
	switch payload[box.payloadStart] {
	case 0:
		return binary.BigEndian.Uint32(payload[box.payloadStart+12 : box.payloadStart+16]), nil
	case 1:
		if box.end-box.payloadStart < 32 {
			return 0, errors.New("version-one MP4 media header is truncated")
		}
		return binary.BigEndian.Uint32(payload[box.payloadStart+20 : box.payloadStart+24]), nil
	default:
		return 0, errors.New("MP4 media header version is unsupported")
	}
}

func patchMediaHeaderDuration(payload []byte, box mp4Box, duration uint64) error {
	switch payload[box.payloadStart] {
	case 0:
		if duration > math.MaxUint32 {
			return errors.New("version-zero MP4 media duration overflows")
		}
		binary.BigEndian.PutUint32(payload[box.payloadStart+16:box.payloadStart+20], uint32(duration))
	case 1:
		binary.BigEndian.PutUint64(payload[box.payloadStart+24:box.payloadStart+32], duration)
	default:
		return errors.New("MP4 media header version is unsupported")
	}
	return nil
}

func patchVideoEditList(payload []byte, box mp4Box, firstFrameUS, attemptEndUS int64) error {
	entries, count, err := editListEntries(payload, box)
	if err != nil {
		return err
	}
	if firstFrameUS == 0 {
		if count != 1 || int32(binary.BigEndian.Uint32(payload[entries+4:entries+8])) != 0 {
			return errors.New("zero-offset MP4 video edit list is not singular")
		}
		binary.BigEndian.PutUint32(payload[entries:entries+4], uint32(attemptEndUS))
		return nil
	}
	if count != 2 || int32(binary.BigEndian.Uint32(payload[entries+4:entries+8])) != -1 {
		return errors.New("offset MP4 video edit list is missing its empty leading edit")
	}
	second := entries + 12
	if int32(binary.BigEndian.Uint32(payload[second+4:second+8])) != 0 {
		return errors.New("offset MP4 video media edit is invalid")
	}
	binary.BigEndian.PutUint32(payload[entries:entries+4], uint32(firstFrameUS))
	binary.BigEndian.PutUint32(payload[second:second+4], uint32(attemptEndUS-firstFrameUS))
	return nil
}

func patchAudioEditList(payload []byte, box mp4Box, attemptEndUS int64) error {
	entries, count, err := editListEntries(payload, box)
	if err != nil {
		return err
	}
	if count != 1 || int32(binary.BigEndian.Uint32(payload[entries+4:entries+8])) < 0 {
		return errors.New("MP4 audio edit list does not preserve one AAC priming edit")
	}
	binary.BigEndian.PutUint32(payload[entries:entries+4], uint32(attemptEndUS))
	return nil
}
