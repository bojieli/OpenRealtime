package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// runEfficiency measures the costs the design claims are small.
//
// Every one of these is a release gate rather than a comparative claim, and
// each is stated against a declared machine. An efficiency number without the
// machine it was measured on is not a number, and putting the machine in a
// footnote is how it stops being read.
func runEfficiency(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime efficiency", flag.ContinueOnError)
	var (
		out     string
		seconds float64
		width   int
		height  int
		fps     float64
	)
	flags.StringVar(&out, "out", "", "write the report to this path as JSON")
	flags.Float64Var(&seconds, "seconds", 60, "how much simulated video time to measure")
	flags.IntVar(&width, "width", 1920, "video source width")
	flags.IntVar(&height, "height", 1080, "video source height")
	flags.Float64Var(&fps, "fps", 3, "frames per second a client sends")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	report := efficiencyReport{
		Machine:  describeMachine(),
		Measured: time.Now().UTC().Format(time.RFC3339),
		Source:   fmt.Sprintf("%dx%d at %.0f fps for %.0f s", width, height, fps, seconds),
	}
	idle, err := measureIdleSource(seconds, width, height, fps)
	if err != nil {
		return err
	}
	report.IdleVideoSource = idle

	active, err := measureActiveSource(seconds, width, height, fps)
	if err != nil {
		return err
	}
	report.ChangingVideoSource = active

	report.Bandwidth = measureBandwidth(idle, active, fps)
	report.ContextGrowth = measureContextGrowth(active)
	report.AudioGate = measureAudioGate()

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(output, string(encoded))
	return err
}

type efficiencyReport struct {
	Machine  machine `json:"machine"`
	Measured string  `json:"measured_at"`
	Source   string  `json:"source"`

	IdleVideoSource     sourceCost    `json:"idle_video_source"`
	ChangingVideoSource sourceCost    `json:"changing_video_source"`
	Bandwidth           bandwidthCost `json:"bandwidth"`
	ContextGrowth       contextCost   `json:"context_growth"`
	AudioGate           gateCost      `json:"audio_gate"`
}

type machine struct {
	CPU       string `json:"cpu"`
	Cores     int    `json:"cores"`
	GPU       string `json:"gpu,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	GoVersion string `json:"go_version"`
}

type sourceCost struct {
	Frames          int     `json:"frames"`
	AdmittedByGate  int     `json:"admitted_by_gate"`
	Narrated        int     `json:"narrated"`
	GateNanoseconds float64 `json:"gate_ns_per_frame"`
	ObserveMillis   float64 `json:"observe_ms_per_admitted_frame"`
	IdleFraction    float64 `json:"idle_fraction"`
}

type bandwidthCost struct {
	FrameBytes               int     `json:"mean_frame_bytes"`
	UngatedKilobytesPerS     float64 `json:"ungated_kb_per_second"`
	ServerGatedKilobytesPerS float64 `json:"server_gated_kb_per_second"`
	ClientGatedKilobytesPerS float64 `json:"client_gated_kb_per_second"`
}

type contextCost struct {
	NarrationBytes        int     `json:"narration_bytes"`
	TrajectoryBytesPerMin float64 `json:"trajectory_bytes_per_minute"`
	SnapshotBytes         int     `json:"snapshot_bytes_after_one_minute"`
}

type gateCost struct {
	Nanoseconds  float64 `json:"ns_per_20ms_frame"`
	RealtimeCost float64 `json:"fraction_of_realtime"`
}

func describeMachine() machine {
	result := machine{
		Cores: runtime.NumCPU(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		GoVersion: runtime.Version(),
	}
	if payload, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(payload), "\n") {
			if name, value, found := strings.Cut(line, ":"); found &&
				strings.TrimSpace(name) == "model name" {
				result.CPU = strings.TrimSpace(value)
				break
			}
		}
	}
	command := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	if payload, err := command.Output(); err == nil {
		result.GPU = strings.TrimSpace(strings.Split(string(payload), "\n")[0])
	}
	return result
}

// measureIdleSource is the headline number: what a screen nobody is touching
// costs. The client keeps sending; the gate is what stops it being expensive.
func measureIdleSource(seconds float64, width, height int, fps float64) (sourceCost, error) {
	frame, err := encodeScreen(width, height, 0, false)
	if err != nil {
		return sourceCost{}, err
	}
	narrator := &countingNarrator{}
	// The observer measures its sampling interval against a clock rather than
	// against the timestamps a client wrote, so a simulated minute needs a
	// simulated clock: run in real time and the whole minute would arrive in
	// microseconds and the cadence would reject all of it.
	simulated := clock.NewVirtual(0)
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: narrator, Cadence: time.Duration(float64(time.Second) / fps),
		Now: simulated.NowNS,
	})
	if err != nil {
		return sourceCost{}, err
	}
	return runSource(observer, narrator, simulated, seconds, fps, func(int) ([]byte, error) {
		// An idle screen re-encodes to identical bytes, which is exactly the
		// case the cheap gate exists for.
		return frame, nil
	})
}

// measureActiveSource is the other end: a screen that changes every sample.
func measureActiveSource(seconds float64, width, height int, fps float64) (sourceCost, error) {
	narrator := &countingNarrator{}
	simulated := clock.NewVirtual(0)
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: narrator, Cadence: time.Duration(float64(time.Second) / fps),
		Now: simulated.NowNS,
	})
	if err != nil {
		return sourceCost{}, err
	}
	return runSource(observer, narrator, simulated, seconds, fps, func(index int) ([]byte, error) {
		return encodeScreen(width, height, index, true)
	})
}

func runSource(
	observer *perception.VideoObserver, narrator *countingNarrator, simulated *clock.Virtual,
	seconds, fps float64, frameFor func(int) ([]byte, error),
) (sourceCost, error) {
	count := int(seconds * fps)
	interval := uint64(float64(time.Second) / fps)
	ctx := context.Background()

	var gateElapsed, observeElapsed time.Duration
	var admitted int
	for index := 0; index < count; index++ {
		payload, err := frameFor(index)
		if err != nil {
			return sourceCost{}, err
		}
		if err := simulated.AdvanceToNS(uint64(index) * interval); err != nil {
			return sourceCost{}, err
		}
		frame := perception.Frame{
			Kind: perception.FrameImage, Source: "screen", CapturedNS: uint64(index) * interval,
			Image: payload, MIMEType: "image/jpeg", Width: 1920, Height: 1080,
		}
		started := time.Now()
		open := observer.Gate(frame)
		gateElapsed += time.Since(started)
		if !open {
			continue
		}
		admitted++
		started = time.Now()
		if _, err := observer.Observe(ctx, []perception.Frame{frame}); err != nil {
			return sourceCost{}, err
		}
		observeElapsed += time.Since(started)
	}

	metrics := observer.Metrics()
	result := sourceCost{
		Frames: count, AdmittedByGate: admitted, Narrated: narrator.calls,
		GateNanoseconds: float64(gateElapsed.Nanoseconds()) / float64(max(count, 1)),
		IdleFraction:    float64(count-int(metrics.Narrations)) / float64(max(count, 1)),
	}
	if admitted > 0 {
		result.ObserveMillis = float64(observeElapsed.Milliseconds()) / float64(admitted)
	}
	return result, nil
}

// measureBandwidth compares what the wire carries under three arrangements.
//
// The middle column is what this system does: the client sends everything and
// the server gates. The right column is what a client-side gate would save,
// and it is the number that says whether pushing the gate into clients would
// have been worth what it costs - which is selective perception no longer
// being a property of the server.
func measureBandwidth(idle, active sourceCost, fps float64) bandwidthCost {
	frame, err := encodeScreen(1920, 1080, 0, false)
	if err != nil {
		return bandwidthCost{}
	}
	frameBytes := len(frame)
	ungated := float64(frameBytes) * fps / 1024
	return bandwidthCost{
		FrameBytes:           frameBytes,
		UngatedKilobytesPerS: ungated,
		// The server gate saves compute and context, not bandwidth: the
		// client already sent the frame.
		ServerGatedKilobytesPerS: ungated,
		ClientGatedKilobytesPerS: ungated * float64(idle.AdmittedByGate) /
			float64(max(idle.Frames, 1)),
	}
}

// measureContextGrowth answers what a minute of narration adds to the
// trajectory, which is what gets copied for every continuation request.
func measureContextGrowth(active sourceCost) contextCost {
	const narration = "A settings page is open. A dialog reads: Confirm payment of $40.00 to Acme Ltd, " +
		"with Confirm and Cancel buttons."
	store := trajectory.NewStore()
	previous := ""
	for index := 0; index < max(active.Narrated, 1); index++ {
		item := trajectory.Item{
			ID: fmt.Sprintf("obs-%d", index), Kind: trajectory.KindObservation,
			MonotonicNS: uint64(index), SourceRevision: uint64(index + 1),
			Producer: trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: "video"},
			Content:  narration,
			Observation: &trajectory.ObservationMeta{
				Observer: "video", Source: "screen", Authority: trajectory.AuthorityObserver,
				Media: []trajectory.MediaRef{{
					Handle: fmt.Sprintf("media-%d", index), MIMEType: "image/jpeg",
					Width: 1920, Height: 1080, Bytes: 120_000,
				}},
			},
		}
		if previous != "" {
			item.CausalParentIDs = []string{previous}
		}
		if store.Append(item) != nil {
			break
		}
		previous = item.ID
	}
	snapshot := store.Snapshot()
	encoded, _ := json.Marshal(snapshot)
	return contextCost{
		NarrationBytes:        len(narration),
		TrajectoryBytesPerMin: float64(len(encoded)),
		SnapshotBytes:         len(encoded),
	}
}

// measureAudioGate is the same question for the path that runs continuously.
func measureAudioGate() gateCost {
	gate, err := perception.NewEnergyGate(perception.DefaultGateConfig(), 24_000)
	if err != nil {
		return gateCost{}
	}
	const frames = 5_000
	block := make([]byte, 480*2) // 20 ms at 24 kHz
	for index := range block {
		block[index] = byte(index)
	}
	started := time.Now()
	for index := 0; index < frames; index++ {
		if _, err := gate.Push(block); err != nil {
			return gateCost{}
		}
	}
	elapsed := time.Since(started)
	perFrame := float64(elapsed.Nanoseconds()) / frames
	return gateCost{
		Nanoseconds: perFrame,
		// A 20 ms frame has 20 ms of budget. This is what fraction of it the
		// gate spends.
		RealtimeCost: perFrame / float64(20*time.Millisecond),
	}
}

type countingNarrator struct{ calls int }

func (narrator *countingNarrator) Name() string { return "counting" }

func (narrator *countingNarrator) Narrate(
	context.Context, []perception.Frame, trajectory.Snapshot,
) (string, error) {
	narrator.calls++
	return "A settings page is open.", nil
}

// encodeScreen produces a frame that looks like a screen rather than noise:
// mostly flat, with text-like structure, because JPEG size and gate behaviour
// both depend on that.
func encodeScreen(width, height, seed int, changing bool) ([]byte, error) {
	canvas := image.NewGray(image.Rect(0, 0, width, height))
	source := rand.New(rand.NewPCG(1, 1))
	for y := 0; y < height; y++ {
		shade := uint8(240)
		if y%40 < 2 {
			shade = 210
		}
		for x := 0; x < width; x++ {
			canvas.SetGray(x, y, color.Gray{Y: shade})
		}
	}
	// Text-like blocks.
	for block := 0; block < 400; block++ {
		x := source.IntN(width - 200)
		y := source.IntN(height - 20)
		length := 40 + source.IntN(160)
		for offsetY := 0; offsetY < 10; offsetY++ {
			for offsetX := 0; offsetX < length; offsetX++ {
				canvas.SetGray(x+offsetX, y+offsetY, color.Gray{Y: 40})
			}
		}
	}
	if changing {
		// A dialog appearing in a different place each time.
		originX := (seed * 37) % max(width-400, 1)
		originY := (seed * 53) % max(height-200, 1)
		for y := originY; y < originY+200 && y < height; y++ {
			for x := originX; x < originX+400 && x < width; x++ {
				canvas.SetGray(x, y, color.Gray{Y: 120})
			}
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 75}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
