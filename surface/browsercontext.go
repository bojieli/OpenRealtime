package surface

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/computeruse/browser"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// BrowserContext is one browser this surface both watches and drives.
//
// Watching and driving being the same object is the point of it. The protocol
// already says an action targets a declared video source, and that the result
// of an action is the next frame of that source - so a context that captures
// frames and performs clicks against the same page is the smallest thing that
// makes the loop observable. Everything the agent does to this browser shows
// up in what the agent next sees, on the same channel, in the same coordinate
// space, with nothing in between to be wrong about.
//
// This adds nothing to the protocol. The frames are ordinary video-source
// frames under a source name, and the actions are the ordinary computer.*
// vocabulary. A deployment that puts computer use on the server instead gets
// the same session; what it does not get is a page that can show you both
// halves at once.
type BrowserContext struct {
	surface *browser.Surface
	source  string

	mu sync.RWMutex
	// target and dispatcher move together, and that is why they are behind one
	// lock rather than two. The dispatcher validates a coordinate against the
	// target it was built with, so a target that changed and a dispatcher that
	// did not is a page whose actions are checked against a screen that is no
	// longer there.
	target     computeruse.Target
	dispatcher *computeruse.Dispatcher
	lastURL    string
	captured   int
	acted      int
}

// BrowserConfig configures the context.
type BrowserConfig struct {
	// DevToolsURL is the browser's debugging endpoint, such as
	// http://127.0.0.1:9222.
	DevToolsURL string
	// TargetURL connects directly to a known page WebSocket instead.
	TargetURL string
	// Source names the video source these frames arrive under. It defaults to
	// "browser", which is what makes this channel distinguishable from the
	// screen the person shares and the camera pointed at the room - three
	// video sources, three meanings, and a coordinate is only well-defined
	// against one of them.
	Source string
	// StartURL is navigated to on connect. Empty leaves the page where it is.
	StartURL string
	// Timeout bounds one command.
	Timeout time.Duration
}

// ConnectBrowser opens the context.
func ConnectBrowser(ctx context.Context, config BrowserConfig) (*BrowserContext, error) {
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	source := strings.TrimSpace(config.Source)
	if source == "" {
		source = "browser"
	}
	surface, err := browser.Connect(ctx, browser.Config{
		DevToolsURL: config.DevToolsURL, TargetURL: config.TargetURL, Timeout: config.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("connect the browser context: %w", err)
	}
	if start := strings.TrimSpace(config.StartURL); start != "" {
		if err := surface.Navigate(ctx, start); err != nil {
			_ = surface.Close()
			return nil, fmt.Errorf("navigate to %s: %w", start, err)
		}
	}
	width, height, err := surface.Viewport(ctx)
	if err != nil {
		_ = surface.Close()
		return nil, fmt.Errorf("read the browser viewport: %w", err)
	}

	page := &BrowserContext{surface: surface, source: source}
	if err := page.retarget(width, height); err != nil {
		_ = surface.Close()
		return nil, err
	}
	if location, err := surface.Location(ctx); err == nil {
		page.lastURL = location
	}
	return page, nil
}

// retarget declares the coordinate space and builds the dispatcher that
// enforces it.
//
// The target owns exactly the one source it can actually show, so an action
// naming the shared screen or the camera is refused before it reaches the
// browser. That is worth having even on a developer's own machine: the camera
// is pointed at a room, and a model that has confused the room with the page
// should discover that as a refusal rather than as a click.
func (page *BrowserContext) retarget(width, height int) error {
	target := computeruse.Target{
		Name: "surface-browser", Sources: []string{page.source}, Width: width, Height: height,
	}
	dispatcher, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{
		Target: target, Surface: page.surface,
		Audit: func(computeruse.Record) {
			page.mu.Lock()
			page.acted++
			page.mu.Unlock()
		},
	})
	if err != nil {
		return err
	}
	page.mu.Lock()
	page.target, page.dispatcher = target, dispatcher
	page.mu.Unlock()
	return nil
}

// Source is the video source name frames arrive under.
func (page *BrowserContext) Source() string { return page.source }

// TargetName is the declared computer-use target.
func (page *BrowserContext) TargetName() string { return page.Target().Name }

// Target is the declared context, for a caller building tool specifications.
func (page *BrowserContext) Target() computeruse.Target {
	page.mu.RLock()
	defer page.mu.RUnlock()
	return page.target
}

// Viewport is the coordinate space actions are expressed in.
func (page *BrowserContext) Viewport() (width, height int) {
	target := page.Target()
	return target.Width, target.Height
}

// LastURL is where the page was when it was last looked at.
func (page *BrowserContext) LastURL() string {
	page.mu.RLock()
	defer page.mu.RUnlock()
	return page.lastURL
}

// Counts reports how much has happened, for the page's channel meters.
func (page *BrowserContext) Counts() (captured, acted int) {
	page.mu.RLock()
	defer page.mu.RUnlock()
	return page.captured, page.acted
}

// CaptureFrame returns one frame, base64 encoded, with the geometry it was
// captured in.
//
// The geometry is re-read on every capture rather than cached from connect,
// and a change rebuilds the dispatcher along with the declaration. A
// coordinate space the client believes and the browser has left behind is the
// one failure in computer use that produces a plausible click on the wrong
// thing - and a dispatcher still bounding actions by the old size would keep
// admitting them.
func (page *BrowserContext) CaptureFrame(ctx context.Context) (string, int, int, error) {
	width, height, err := page.surface.Viewport(ctx)
	if err != nil {
		return "", 0, 0, fmt.Errorf("read the viewport: %w", err)
	}
	if current := page.Target(); current.Width != width || current.Height != height {
		if err := page.retarget(width, height); err != nil {
			return "", 0, 0, fmt.Errorf("redeclare the coordinate space: %w", err)
		}
	}
	image, err := page.surface.Capture(ctx)
	if err != nil {
		return "", 0, 0, fmt.Errorf("capture the page: %w", err)
	}
	location, _ := page.surface.Location(ctx)
	page.mu.Lock()
	page.captured++
	if location != "" {
		page.lastURL = location
	}
	page.mu.Unlock()
	return base64.StdEncoding.EncodeToString(image), width, height, nil
}

// Act performs one computer-use action and returns the text the session should
// receive.
//
// The output stays text - "clicked" - and no image is ever carried in a
// function result. The visual consequence returns through the video stream on
// the next capture, exactly as the specification says and exactly as it does
// for a person, who also does not receive a screenshot in reply to moving
// their hand.
func (page *BrowserContext) Act(
	ctx context.Context, callID, name string, arguments json.RawMessage,
) (string, error) {
	if !computeruse.IsAction(name) {
		return "", fmt.Errorf("%q is not a computer-use action", name)
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	page.mu.RLock()
	dispatcher := page.dispatcher
	page.mu.RUnlock()

	result, err := dispatcher.Dispatch(ctx, trajectory.ToolCall{
		CallID: callID, Name: name, Arguments: arguments,
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Error) != "" {
		return "", errors.New(result.Error)
	}
	return string(result.Output), nil
}

// Navigate points the page somewhere, for a developer setting up a run.
func (page *BrowserContext) Navigate(ctx context.Context, url string) error {
	if err := page.surface.Navigate(ctx, url); err != nil {
		return err
	}
	page.mu.Lock()
	page.lastURL = url
	page.mu.Unlock()
	return nil
}

// Close ends the connection.
func (page *BrowserContext) Close() error { return page.surface.Close() }
