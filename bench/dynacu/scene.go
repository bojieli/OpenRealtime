// Package dynacu is the computer-use functional gate.
//
// The dynamic computer-use suite itself stays in its own repository, and this
// ships a runner for it rather than a copy. But a release gate that depends on
// a dataset somebody else hosts is a release gate that stops running, so this
// package also carries a small closed-loop environment of its own: a rendered
// screen, a real click, and a screen that changes because of it.
//
// That is deliberately not a benchmark. It answers one question - does video
// observation and action grounding work end to end, over the protocol, with
// nothing faked - and it answers it in a few seconds without a GPU or a
// network.
package dynacu

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"strings"
	"sync"
)

// Region is a rectangle on the screen, in the coordinate space the model sees.
type Region struct {
	X, Y, Width, Height int
}

// Contains reports whether a point is inside.
func (region Region) Contains(x, y int) bool {
	return x >= region.X && x < region.X+region.Width &&
		y >= region.Y && y < region.Y+region.Height
}

// Centre is the point a competent agent would aim at.
func (region Region) Centre() (int, int) {
	return region.X + region.Width/2, region.Y + region.Height/2
}

// Scene is a screen the agent can see and act on.
//
// It is a state machine with a renderer: a dialog is open, the agent clicks
// Confirm, and the next frame shows the confirmation. Nothing about the loop
// is faked - the frames go over the protocol as ordinary video events, the
// click comes back as an ordinary function call, and the frame that follows
// is genuinely different because of it.
type Scene struct {
	Width  int
	Height int

	mu         sync.Mutex
	state      State
	confirm    Region
	cancel     Region
	clicks     []Click
	generation int
}

// State is where the scene has got to.
type State string

const (
	// StateDialog: a confirmation dialog is open.
	StateDialog State = "dialog"
	// StateConfirmed: the agent clicked Confirm.
	StateConfirmed State = "confirmed"
	// StateCancelled: the agent clicked Cancel.
	StateCancelled State = "cancelled"
)

// Click is one action the agent took.
type Click struct {
	X, Y   int
	Inside string
}

// NewScene creates the dialog scene.
func NewScene() *Scene {
	const width, height = 1280, 720
	return &Scene{
		Width: width, Height: height, state: StateDialog,
		confirm: Region{X: 700, Y: 420, Width: 180, Height: 56},
		cancel:  Region{X: 480, Y: 420, Width: 180, Height: 56},
	}
}

// Prompt is what the dialog says, and what a working observer should narrate.
const Prompt = "Confirm payment of $40.00 to Acme Ltd"

// Instruction is what the user asks for out loud.
const Instruction = "Look at the screen and confirm the payment."

// State reports where the scene is.
func (scene *Scene) State() State {
	scene.mu.Lock()
	defer scene.mu.Unlock()
	return scene.state
}

// Clicks reports what the agent did.
func (scene *Scene) Clicks() []Click {
	scene.mu.Lock()
	defer scene.mu.Unlock()
	return append([]Click(nil), scene.clicks...)
}

// Generation increments whenever the screen changes, so a caller can tell a
// new frame from a repeat without comparing bytes.
func (scene *Scene) Generation() int {
	scene.mu.Lock()
	defer scene.mu.Unlock()
	return scene.generation
}

// Click applies an action and returns what it landed on.
func (scene *Scene) Click(x, y int) string {
	scene.mu.Lock()
	defer scene.mu.Unlock()
	target := "background"
	switch {
	case scene.confirm.Contains(x, y):
		target = "confirm"
		if scene.state == StateDialog {
			scene.state = StateConfirmed
			scene.generation++
		}
	case scene.cancel.Contains(x, y):
		target = "cancel"
		if scene.state == StateDialog {
			scene.state = StateCancelled
			scene.generation++
		}
	}
	scene.clicks = append(scene.clicks, Click{X: x, Y: y, Inside: target})
	return target
}

// ConfirmRegion is where a correct click lands, for reporting how far off a
// wrong one was.
func (scene *Scene) ConfirmRegion() Region { return scene.confirm }

// Frame renders the current screen as JPEG.
func (scene *Scene) Frame() ([]byte, error) {
	scene.mu.Lock()
	state := scene.state
	confirm, cancel := scene.confirm, scene.cancel
	scene.mu.Unlock()

	canvas := image.NewRGBA(image.Rect(0, 0, scene.Width, scene.Height))
	fill(canvas, canvas.Bounds(), color.RGBA{R: 242, G: 242, B: 245, A: 255})
	// A window, so the screen looks like a screen rather than a test pattern.
	fill(canvas, image.Rect(300, 200, 980, 520), color.RGBA{R: 255, G: 255, B: 255, A: 255})
	fill(canvas, image.Rect(300, 200, 980, 250), color.RGBA{R: 226, G: 228, B: 233, A: 255})

	switch state {
	case StateDialog:
		writeText(canvas, 330, 300, Prompt, color.RGBA{A: 255})
		writeText(canvas, 330, 330, "This cannot be undone.", color.RGBA{R: 90, G: 90, B: 96, A: 255})
		button(canvas, confirm, color.RGBA{R: 32, G: 120, B: 220, A: 255}, "CONFIRM")
		button(canvas, cancel, color.RGBA{R: 200, G: 200, B: 206, A: 255}, "CANCEL")
	case StateConfirmed:
		writeText(canvas, 330, 300, "Payment sent to Acme Ltd", color.RGBA{R: 20, G: 130, B: 60, A: 255})
		writeText(canvas, 330, 330, "Reference 88150", color.RGBA{R: 90, G: 90, B: 96, A: 255})
	case StateCancelled:
		writeText(canvas, 330, 300, "Payment cancelled", color.RGBA{R: 180, G: 40, B: 40, A: 255})
	}

	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func fill(canvas draw.Image, region image.Rectangle, shade color.Color) {
	draw.Draw(canvas, region, &image.Uniform{C: shade}, image.Point{}, draw.Src)
}

func button(canvas draw.Image, region Region, shade color.Color, label string) {
	fill(canvas, image.Rect(region.X, region.Y, region.X+region.Width, region.Y+region.Height), shade)
	writeText(canvas, region.X+24, region.Y+24, label, color.RGBA{R: 255, G: 255, B: 255, A: 255})
}

// writeText draws readable block glyphs.
//
// A bitmap font rather than a real one: this needs a vision model to be able
// to read the words, not typographic quality, and a font dependency for a
// self-contained gate would defeat the point of it being self-contained.
func writeText(canvas draw.Image, x, y int, text string, shade color.Color) {
	const scale = 3
	cursor := x
	for _, symbol := range strings.ToUpper(text) {
		glyph, known := glyphs[symbol]
		if !known {
			cursor += 4 * scale
			continue
		}
		for row := 0; row < 7; row++ {
			for column := 0; column < 5; column++ {
				if glyph[row]&(1<<(4-column)) == 0 {
					continue
				}
				fill(canvas, image.Rect(
					cursor+column*scale, y+row*scale,
					cursor+(column+1)*scale, y+(row+1)*scale,
				), shade)
			}
		}
		cursor += 6 * scale
	}
}

// Validate checks the scene is usable.
func (scene *Scene) Validate() error {
	if scene.Width <= 0 || scene.Height <= 0 {
		return errors.New("a scene needs a coordinate space")
	}
	if _, err := scene.Frame(); err != nil {
		return fmt.Errorf("render the scene: %w", err)
	}
	return nil
}
