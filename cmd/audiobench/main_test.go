package main

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/audiobench"
)

func TestRunRequiresAStage(t *testing.T) {
	err := run([]string{"--output", t.TempDir() + "/report.json"})
	if err == nil || !strings.Contains(err.Error(), "at least one stage") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunValidatesDurationsBeforeProviders(t *testing.T) {
	err := run([]string{"--asr-audio", "missing.wav", "--stage-timeout", "0s"})
	if err == nil || !strings.Contains(err.Error(), "timeouts") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunRejectsNegativeASRProviderChunk(t *testing.T) {
	err := run([]string{"--asr-audio", "missing.wav", "--asr-provider-chunk", "-1ms"})
	if err == nil || !strings.Contains(err.Error(), "provider chunk") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunOpenAISpeechWritesMeasuredReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/audio/speech" {
			http.NotFound(writer, request)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload["stream"] != true || payload["response_format"] != "pcm" || payload["seed"] != float64(7) {
			t.Errorf("payload = %+v", payload)
		}
		writer.Header().Set("Content-Type", "audio/pcm")
		writer.Header().Set("X-Sample-Rate", "24000")
		audio := make([]byte, 480)
		for index := 0; index < len(audio)/2; index++ {
			binary.LittleEndian.PutUint16(audio[index*2:], uint16(index))
		}
		_, _ = writer.Write(audio)
	}))
	defer server.Close()

	directory := t.TempDir()
	reportPath := filepath.Join(directory, "report.json")
	wavPath := filepath.Join(directory, "audio", "result.wav")
	err := run([]string{
		"--tts-text", "hello", "--tts-provider", "openai-speech",
		"--tts-url", server.URL + "/v1/audio/speech", "--output", reportPath,
		"--tts-wav", wavPath, "--tts-deterministic", "--tts-seed", "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report audiobench.Report
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if report.TTS == nil || report.TTS.OutputBytes != 480 {
		t.Fatalf("TTS report = %+v", report.TTS)
	}
	if report.Runtime["tts_transport"] != "openai-compatible-speech-http" || report.Runtime["wire_protocol"] != "unchanged-openai-realtime" || report.Runtime["tts_sampling_seed"] != "7" {
		t.Fatalf("runtime = %+v", report.Runtime)
	}
	if _, err := os.Stat(wavPath); err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsUnknownOrIncompatibleTTSProviderOptions(t *testing.T) {
	err := run([]string{"--tts-text", "hello", "--tts-provider", "unknown", "--output", t.TempDir() + "/report.json"})
	if err == nil || !strings.Contains(err.Error(), "unsupported --tts-provider") {
		t.Fatalf("unknown provider error = %v", err)
	}
	err = run([]string{
		"--tts-text", "hello", "--tts-provider", "fish-native",
		"--tts-reference-audio", "voice.wav", "--tts-reference-text", "hello",
		"--output", t.TempDir() + "/report.json",
	})
	if err == nil || !strings.Contains(err.Error(), "openai-speech") {
		t.Fatalf("incompatible option error = %v", err)
	}
}
