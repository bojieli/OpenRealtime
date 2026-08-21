// Package browser performs computer-use actions in a browser context.
//
// It speaks the Chrome DevTools Protocol over a WebSocket, which needs no
// driver binary, no cgo, and nothing on the machine except a Chromium that is
// already listening. A browser context is also the right default target:
// blast radius is bounded by what the browser can reach, and nothing here can
// touch an ambient desktop.
//
// The surface it implements is nine methods wide and knows nothing about
// authority, confirmation, or auditing. Those live above it, which is what
// lets this file be only about making the browser do things.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/coder/websocket"
)

// Config configures the connection.
type Config struct {
	// DevToolsURL is the browser's debugging endpoint, such as
	// http://127.0.0.1:9222. A page target is discovered through it.
	DevToolsURL string
	// TargetURL connects directly to a known page WebSocket instead, which is
	// what a deployment that manages its own tabs will have.
	TargetURL string
	// Timeout bounds one command.
	Timeout time.Duration
	// TypingDelay paces synthetic keystrokes. Zero types instantly, which is
	// correct for a form and wrong for a page that debounces input.
	TypingDelay time.Duration
}

// Surface drives one browser page.
type Surface struct {
	config     Config
	connection *websocket.Conn

	writeMu   sync.Mutex
	sequence  atomic.Int64
	pendingMu sync.Mutex
	pending   map[int64]chan json.RawMessage

	closed atomic.Bool
	done   chan struct{}
	once   sync.Once
}

// Connect opens a page and returns a surface over it.
func Connect(ctx context.Context, config Config) (*Surface, error) {
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	target := strings.TrimSpace(config.TargetURL)
	if target == "" {
		if strings.TrimSpace(config.DevToolsURL) == "" {
			return nil, errors.New("a browser surface needs a DevTools URL or a target URL")
		}
		discovered, err := discover(ctx, config.DevToolsURL, config.Timeout)
		if err != nil {
			return nil, err
		}
		target = discovered
	}
	connection, _, err := websocket.Dial(ctx, target, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to the browser page: %w", err)
	}
	connection.SetReadLimit(32 << 20)
	surface := &Surface{
		config: config, connection: connection,
		pending: make(map[int64]chan json.RawMessage), done: make(chan struct{}),
	}
	go surface.read()
	return surface, nil
}

// Name identifies the surface in audit records.
func (surface *Surface) Name() string { return "browser" }

// Close ends the connection.
func (surface *Surface) Close() error {
	if surface.closed.Swap(true) {
		return nil
	}
	surface.once.Do(func() { close(surface.done) })
	return surface.connection.Close(websocket.StatusNormalClosure, "closed")
}

type devToolsTarget struct {
	Type                 string `json:"type"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func discover(ctx context.Context, base string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, strings.TrimRight(base, "/")+"/json/list", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("list browser targets: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var targets []devToolsTarget
	if err := json.Unmarshal(payload, &targets); err != nil {
		return "", fmt.Errorf("decode browser targets: %w", err)
	}
	for _, target := range targets {
		if target.Type == "page" && target.WebSocketDebuggerURL != "" {
			return target.WebSocketDebuggerURL, nil
		}
	}
	return "", errors.New("the browser has no page target to act on")
}

type command struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

type reply struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (surface *Surface) read() {
	for {
		_, payload, err := surface.connection.Read(context.Background())
		if err != nil {
			surface.failPending(err)
			return
		}
		var message reply
		if json.Unmarshal(payload, &message) != nil || message.ID == 0 {
			// An event rather than a reply. This surface issues commands and
			// does not subscribe to anything, so events are not its business.
			continue
		}
		surface.pendingMu.Lock()
		waiter, exists := surface.pending[message.ID]
		delete(surface.pending, message.ID)
		surface.pendingMu.Unlock()
		if !exists {
			continue
		}
		if message.Error != nil {
			waiter <- json.RawMessage(`{"__error":` + strconv.Quote(message.Error.Message) + `}`)
			continue
		}
		waiter <- message.Result
	}
}

func (surface *Surface) failPending(cause error) {
	surface.pendingMu.Lock()
	defer surface.pendingMu.Unlock()
	for id, waiter := range surface.pending {
		waiter <- json.RawMessage(`{"__error":` + strconv.Quote(cause.Error()) + `}`)
		delete(surface.pending, id)
	}
}

func (surface *Surface) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if surface.closed.Load() {
		return nil, errors.New("browser surface is closed")
	}
	id := surface.sequence.Add(1)
	waiter := make(chan json.RawMessage, 1)
	surface.pendingMu.Lock()
	surface.pending[id] = waiter
	surface.pendingMu.Unlock()

	encoded, err := json.Marshal(command{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	timed, cancel := context.WithTimeout(ctx, surface.config.Timeout)
	defer cancel()
	surface.writeMu.Lock()
	writeErr := surface.connection.Write(timed, websocket.MessageText, encoded)
	surface.writeMu.Unlock()
	if writeErr != nil {
		return nil, fmt.Errorf("%s: %w", method, writeErr)
	}
	select {
	case result := <-waiter:
		var failure struct {
			Error string `json:"__error"`
		}
		if json.Unmarshal(result, &failure) == nil && failure.Error != "" {
			return nil, fmt.Errorf("%s: %s", method, failure.Error)
		}
		return result, nil
	case <-timed.Done():
		surface.pendingMu.Lock()
		delete(surface.pending, id)
		surface.pendingMu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, timed.Err())
	}
}

// Click presses and releases a mouse button at a point.
func (surface *Surface) Click(ctx context.Context, x, y int, button string) error {
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		if _, err := surface.call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": kind, "x": x, "y": y, "button": button, "clickCount": 1,
		}); err != nil {
			return err
		}
	}
	return nil
}

// DoubleClick sends a two-click sequence.
func (surface *Surface) DoubleClick(ctx context.Context, x, y int) error {
	if err := surface.Click(ctx, x, y, "left"); err != nil {
		return err
	}
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		if _, err := surface.call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": kind, "x": x, "y": y, "button": "left", "clickCount": 2,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Move moves the pointer without pressing anything.
func (surface *Surface) Move(ctx context.Context, x, y int) error {
	_, err := surface.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved", "x": x, "y": y,
	})
	return err
}

// Drag presses at one point, moves, and releases at another.
func (surface *Surface) Drag(ctx context.Context, fromX, fromY, toX, toY int) error {
	steps := []map[string]any{
		{"type": "mousePressed", "x": fromX, "y": fromY, "button": "left", "clickCount": 1},
		// An intermediate move matters: a page that tracks dragging sees
		// nothing if the pointer teleports.
		{"type": "mouseMoved", "x": (fromX + toX) / 2, "y": (fromY + toY) / 2, "button": "left"},
		{"type": "mouseMoved", "x": toX, "y": toY, "button": "left"},
		{"type": "mouseReleased", "x": toX, "y": toY, "button": "left", "clickCount": 1},
	}
	for _, step := range steps {
		if _, err := surface.call(ctx, "Input.dispatchMouseEvent", step); err != nil {
			return err
		}
	}
	return nil
}

// Type inserts literal text.
func (surface *Surface) Type(ctx context.Context, text string) error {
	if surface.config.TypingDelay <= 0 {
		_, err := surface.call(ctx, "Input.insertText", map[string]any{"text": text})
		return err
	}
	for _, symbol := range text {
		if _, err := surface.call(ctx, "Input.insertText", map[string]any{
			"text": string(symbol),
		}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(surface.config.TypingDelay):
		}
	}
	return nil
}

// Key presses a combination of keys together.
func (surface *Surface) Key(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return errors.New("a key action requires at least one key")
	}
	modifiers := 0
	var main string
	for _, key := range keys {
		switch strings.ToLower(key) {
		case "alt":
			modifiers |= 1
		case "ctrl", "control":
			modifiers |= 2
		case "meta", "cmd", "command":
			modifiers |= 4
		case "shift":
			modifiers |= 8
		default:
			main = key
		}
	}
	if main == "" {
		return errors.New("a key action requires a non-modifier key")
	}
	for _, kind := range []string{"keyDown", "keyUp"} {
		if _, err := surface.call(ctx, "Input.dispatchKeyEvent", map[string]any{
			"type": kind, "modifiers": modifiers, "key": main,
			"windowsVirtualKeyCode": virtualKeyCode(main), "text": keyText(main, modifiers),
		}); err != nil {
			return err
		}
	}
	return nil
}

// Scroll scrolls at a point.
func (surface *Surface) Scroll(ctx context.Context, x, y, deltaX, deltaY int) error {
	_, err := surface.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": deltaX, "deltaY": deltaY,
	})
	return err
}

// Screenshot captures the page.
//
// The bytes are discarded here rather than returned, and deliberately: the
// result of an action is the next screen, and the screen arrives through the
// video stream. Carrying an image back through a function result would put the
// same picture on two paths.
func (surface *Surface) Screenshot(ctx context.Context) error {
	_, err := surface.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 80})
	return err
}

// Capture returns a screenshot, for a caller that is feeding the video stream
// rather than acting.
func (surface *Surface) Capture(ctx context.Context) ([]byte, error) {
	result, err := surface.call(ctx, "Page.captureScreenshot", map[string]any{
		"format": "jpeg", "quality": 80,
	})
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(decoded.Data)
}

// Viewport reports the page's coordinate space, which is what a computer-use
// target must declare.
func (surface *Surface) Viewport(ctx context.Context) (width, height int, err error) {
	result, err := surface.call(ctx, "Page.getLayoutMetrics", nil)
	if err != nil {
		return 0, 0, err
	}
	var decoded struct {
		CSSLayoutViewport struct {
			ClientWidth  int `json:"clientWidth"`
			ClientHeight int `json:"clientHeight"`
		} `json:"cssLayoutViewport"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return 0, 0, err
	}
	return decoded.CSSLayoutViewport.ClientWidth, decoded.CSSLayoutViewport.ClientHeight, nil
}

func virtualKeyCode(key string) int {
	switch strings.ToLower(key) {
	case "enter", "return":
		return 13
	case "tab":
		return 9
	case "escape", "esc":
		return 27
	case "backspace":
		return 8
	case "delete":
		return 46
	case "arrowup", "up":
		return 38
	case "arrowdown", "down":
		return 40
	case "arrowleft", "left":
		return 37
	case "arrowright", "right":
		return 39
	default:
		if len(key) == 1 {
			return int(strings.ToUpper(key)[0])
		}
		return 0
	}
}

func keyText(key string, modifiers int) string {
	// Text is what a page receives as input. A modified key or a named key
	// produces none: pressing ctrl+s must not type an "s".
	if modifiers != 0 || len(key) != 1 {
		return ""
	}
	return key
}

var _ computeruse.Surface = (*Surface)(nil)
