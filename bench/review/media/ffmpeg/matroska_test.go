package ffmpeg

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExactMatroskaCarriesMicrosecondPTSAndDurations(t *testing.T) {
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	root := t.TempDir()
	paths := []string{"frames/screen/000001.png", "frames/screen/000002.jpg", "frames/screen/000003.png"}
	for index, path := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		frame := image.NewRGBA(image.Rect(0, 0, 64, 48))
		for y := 0; y < 48; y++ {
			for x := 0; x < 64; x++ {
				frame.SetRGBA(x, y, color.RGBA{R: uint8(80 * index), G: 100, B: 200, A: 255})
			}
		}
		var payload bytes.Buffer
		if filepath.Ext(path) == ".jpg" {
			if err := jpeg.Encode(&payload, frame, &jpeg.Options{Quality: 95}); err != nil {
				t.Fatal(err)
			}
		} else if err := png.Encode(&payload, frame); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, payload.Bytes(), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	entries := []timelineEntry{
		{path: paths[0], ptsUS: 125_123, durationUS: 300_333},
		{path: paths[1], ptsUS: 425_456, durationUS: 325_333},
		{path: paths[2], ptsUS: 750_789, durationUS: 499_211},
	}
	output := filepath.Join(root, "timeline.mkv")
	if err := writeExactMatroska(t.Context(), output, root, entries, 64, 48); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), probe, "-v", "error", "-show_entries",
		"stream=time_base,start_time,duration:frame=best_effort_timestamp_time,pkt_duration_time",
		"-show_frames", "-of", "json", output)
	payload, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Frames []struct {
			PTS      string `json:"best_effort_timestamp_time"`
			Duration string `json:"pkt_duration_time"`
		} `json:"frames"`
		Streams []struct {
			TimeBase  string `json:"time_base"`
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Streams) != 1 || envelope.Streams[0].TimeBase != "1/1000000" ||
		envelope.Streams[0].StartTime != "0.125123" ||
		len(envelope.Frames) != 4 || envelope.Frames[0].PTS != "0.125123" ||
		envelope.Frames[1].PTS != "0.425456" || envelope.Frames[2].PTS != "0.750789" ||
		envelope.Frames[2].Duration != "0.499211" || envelope.Frames[3].PTS != "1.250000" ||
		envelope.Frames[3].Duration != "0.001000" {
		t.Fatalf("intermediate probe = %s", payload)
	}
}
