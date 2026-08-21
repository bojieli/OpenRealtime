package browser_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/computeruse/browser"
	"github.com/coder/websocket"
)

// fakeBrowser answers DevTools commands and records what it was asked to do,
// which is the only thing worth asserting about a surface: that the action a
// model requested is the action the browser was told to perform.
type fakeBrowser struct {
	server *httptest.Server

	mu       sync.Mutex
	commands []map[string]any
	fail     string
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	fake := &fakeBrowser{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/list", func(writer http.ResponseWriter, request *http.Request) {
		socket := "ws" + strings.TrimPrefix(fake.server.URL, "http") + "/devtools/page/1"
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"type": "background_page", "webSocketDebuggerUrl": "ws://ignored"},
			{"type": "page", "webSocketDebuggerUrl": socket},
		})
	})
	mux.HandleFunc("/devtools/page/1", func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		ctx := request.Context()
		for {
			_, payload, err := connection.Read(ctx)
			if err != nil {
				return
			}
			var incoming map[string]any
			if json.Unmarshal(payload, &incoming) != nil {
				continue
			}
			fake.mu.Lock()
			fake.commands = append(fake.commands, incoming)
			failure := fake.fail
			fake.mu.Unlock()

			response := map[string]any{"id": incoming["id"]}
			if failure != "" {
				response["error"] = map[string]any{"message": failure}
			} else {
				switch incoming["method"] {
				case "Page.captureScreenshot":
					response["result"] = map[string]any{
						"data": base64.StdEncoding.EncodeToString([]byte("jpeg")),
					}
				case "Page.getLayoutMetrics":
					response["result"] = map[string]any{
						"cssLayoutViewport": map[string]any{"clientWidth": 1280, "clientHeight": 720},
					}
				default:
					response["result"] = map[string]any{}
				}
			}
			encoded, _ := json.Marshal(response)
			if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
				return
			}
		}
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeBrowser) issued() []map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]map[string]any(nil), fake.commands...)
}

func (fake *fakeBrowser) methods() []string {
	var methods []string
	for _, command := range fake.issued() {
		methods = append(methods, command["method"].(string))
	}
	return methods
}

func connect(t *testing.T, fake *fakeBrowser) *browser.Surface {
	t.Helper()
	surface, err := browser.Connect(context.Background(), browser.Config{
		DevToolsURL: fake.server.URL, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = surface.Close() })
	return surface
}

func TestConnectDiscoversAPageTarget(t *testing.T) {
	fake := newFakeBrowser(t)
	surface := connect(t, fake)
	width, height, err := surface.Viewport(context.Background())
	if err != nil {
		t.Fatalf("viewport: %v", err)
	}
	if width != 1280 || height != 720 {
		t.Fatalf("a target must declare the space it acts in, got %dx%d", width, height)
	}
}

func TestClickSendsPressAndRelease(t *testing.T) {
	fake := newFakeBrowser(t)
	surface := connect(t, fake)
	if err := surface.Click(context.Background(), 120, 240, "left"); err != nil {
		t.Fatalf("click: %v", err)
	}
	commands := fake.issued()
	if len(commands) != 2 {
		t.Fatalf("a click is a press and a release, got %d commands", len(commands))
	}
	first := commands[0]["params"].(map[string]any)
	if first["type"] != "mousePressed" || first["x"].(float64) != 120 || first["y"].(float64) != 240 {
		t.Fatalf("the coordinate a model chose must be the coordinate the browser gets: %v", first)
	}
	if commands[1]["params"].(map[string]any)["type"] != "mouseReleased" {
		t.Fatal("a press without a release leaves the button down")
	}
}

func TestDragMovesThroughTheGesture(t *testing.T) {
	fake := newFakeBrowser(t)
	surface := connect(t, fake)
	if err := surface.Drag(context.Background(), 10, 10, 110, 110); err != nil {
		t.Fatalf("drag: %v", err)
	}
	commands := fake.issued()
	if len(commands) != 4 {
		t.Fatalf("a drag needs intermediate movement, got %d commands", len(commands))
	}
	kinds := make([]string, 0, len(commands))
	for _, command := range commands {
		kinds = append(kinds, command["params"].(map[string]any)["type"].(string))
	}
	want := []string{"mousePressed", "mouseMoved", "mouseMoved", "mouseReleased"}
	for index, kind := range want {
		if kinds[index] != kind {
			t.Fatalf("step %d: got %s, want %s", index, kinds[index], kind)
		}
	}
}

// A modified key must not also type its letter: ctrl+s saves, it does not
// insert an "s".
func TestModifiedKeysDoNotTypeText(t *testing.T) {
	fake := newFakeBrowser(t)
	surface := connect(t, fake)
	if err := surface.Key(context.Background(), []string{"ctrl", "s"}); err != nil {
		t.Fatalf("key: %v", err)
	}
	for _, command := range fake.issued() {
		params := command["params"].(map[string]any)
		if params["modifiers"].(float64) != 2 {
			t.Fatalf("the control modifier must be set: %v", params)
		}
		if text, present := params["text"]; present && text != "" {
			t.Fatalf("a modified key must not produce text, got %q", text)
		}
	}

	fake.mu.Lock()
	fake.commands = nil
	fake.mu.Unlock()
	if err := surface.Key(context.Background(), []string{"enter"}); err != nil {
		t.Fatalf("key: %v", err)
	}
	params := fake.issued()[0]["params"].(map[string]any)
	if params["windowsVirtualKeyCode"].(float64) != 13 {
		t.Fatalf("a named key must carry its key code: %v", params)
	}
}

func TestTypingInsertsTextInOneCommandByDefault(t *testing.T) {
	fake := newFakeBrowser(t)
	surface := connect(t, fake)
	if err := surface.Type(context.Background(), "hello"); err != nil {
		t.Fatalf("type: %v", err)
	}
	commands := fake.issued()
	if len(commands) != 1 || commands[0]["method"] != "Input.insertText" {
		t.Fatalf("unexpected commands %v", fake.methods())
	}
	if commands[0]["params"].(map[string]any)["text"] != "hello" {
		t.Fatal("the literal text must be inserted")
	}
}

func TestScreenshotDoesNotReturnBytesThroughTheActionPath(t *testing.T) {
	fake := newFakeBrowser(t)
	surface := connect(t, fake)
	// The action returns nothing: the result of an action is the next screen,
	// and the screen arrives through the video stream.
	if err := surface.Screenshot(context.Background()); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	// A caller feeding the video stream asks for the bytes explicitly.
	payload, err := surface.Capture(context.Background())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if string(payload) != "jpeg" {
		t.Fatalf("unexpected capture %q", payload)
	}
}

func TestBrowserErrorsAreReportedNotSwallowed(t *testing.T) {
	fake := newFakeBrowser(t)
	fake.mu.Lock()
	fake.fail = "Cannot find context with specified id"
	fake.mu.Unlock()
	surface := connect(t, fake)
	err := surface.Click(context.Background(), 10, 10, "left")
	if err == nil || !strings.Contains(err.Error(), "Cannot find context") {
		t.Fatalf("a browser failure must reach the caller, got %v", err)
	}
}

func TestConnectRequiresSomewhereToConnect(t *testing.T) {
	if _, err := browser.Connect(context.Background(), browser.Config{}); err == nil {
		t.Fatal("a surface with no browser has nothing to act on")
	}
}

func TestAClosedSurfaceRefusesActions(t *testing.T) {
	fake := newFakeBrowser(t)
	surface := connect(t, fake)
	if err := surface.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := surface.Click(context.Background(), 1, 1, "left"); err == nil {
		t.Fatal("a closed surface must refuse rather than appear to act")
	}
}
