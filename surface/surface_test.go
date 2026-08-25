package surface_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/console"
	"github.com/bojieli/OpenRealtime/surface"
	"github.com/coder/websocket"
)

// --- a browser to observe and act on ----------------------------------------

// fakeBrowser answers DevTools commands and records them. What is worth
// asserting about the browser channel is that the frame the page displayed and
// the click the model asked for reached the same page, so this records both
// sides of that.
type fakeBrowser struct {
	server *httptest.Server

	mu       sync.Mutex
	commands []map[string]any
	width    int
	height   int
	location string
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	fake := &fakeBrowser{width: 1280, height: 720, location: "https://example.test/start"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/list", func(writer http.ResponseWriter, _ *http.Request) {
		socket := "ws" + strings.TrimPrefix(fake.server.URL, "http") + "/devtools/page/1"
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode([]map[string]any{
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
			width, height, location := fake.width, fake.height, fake.location
			fake.mu.Unlock()

			response := map[string]any{"id": incoming["id"], "result": map[string]any{}}
			switch incoming["method"] {
			case "Page.captureScreenshot":
				response["result"] = map[string]any{
					"data": base64.StdEncoding.EncodeToString([]byte("jpeg-bytes")),
				}
			case "Page.getLayoutMetrics":
				response["result"] = map[string]any{
					"cssLayoutViewport": map[string]any{"clientWidth": width, "clientHeight": height},
				}
			case "Runtime.evaluate":
				value := location
				if parameters, ok := incoming["params"].(map[string]any); ok {
					if parameters["expression"] == "document.readyState" {
						value = "complete"
					}
				}
				response["result"] = map[string]any{"result": map[string]any{"value": value}}
			}
			encoded, _ := json.Marshal(response)
			if connection.Write(ctx, websocket.MessageText, encoded) != nil {
				return
			}
		}
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeBrowser) performed(method string) []map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var matched []map[string]any
	for _, command := range fake.commands {
		if command["method"] == method {
			matched = append(matched, command)
		}
	}
	return matched
}

func (fake *fakeBrowser) resize(width, height int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.width, fake.height = width, height
}

func connectFakeBrowser(t *testing.T) *surface.BrowserContext {
	t.Helper()
	fake := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	context, err := surface.ConnectBrowser(ctx, surface.BrowserConfig{DevToolsURL: fake.server.URL})
	if err != nil {
		t.Fatalf("connect the browser context: %v", err)
	}
	t.Cleanup(func() { _ = context.Close() })
	return context
}

// --- a stand-in endpoint ----------------------------------------------------

type endpoint struct {
	server *httptest.Server
	token  chan string
	got    chan string
	send   chan string
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	fake := &endpoint{
		token: make(chan string, 4), got: make(chan string, 64), send: make(chan string, 64),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			fake.token <- request.Header.Get("Authorization")
			connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
			if err != nil {
				return
			}
			connection.SetReadLimit(16 << 20)
			defer connection.CloseNow()
			ctx := request.Context()
			go func() {
				for {
					_, payload, err := connection.Read(ctx)
					if err != nil {
						return
					}
					fake.got <- string(payload)
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case message := <-fake.send:
					if connection.Write(ctx, websocket.MessageText, []byte(message)) != nil {
						return
					}
				}
			}
		}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *endpoint) url() string { return "ws" + strings.TrimPrefix(fake.server.URL, "http") }

func startSurface(t *testing.T, config surface.Config) *httptest.Server {
	t.Helper()
	server, err := surface.New(config)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	local := httptest.NewServer(server.Handler())
	t.Cleanup(local.Close)
	return local
}

func fileHost(t *testing.T) *console.Host {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello from disk"), 0o600); err != nil {
		t.Fatalf("seed the root: %v", err)
	}
	host, err := console.NewHost(console.HostConfig{
		Root: root, Enabled: []string{"read_file", "list_directory", "write_file"},
	})
	if err != nil {
		t.Fatalf("tool host: %v", err)
	}
	return host
}

// --- artifacts --------------------------------------------------------------

// An artifact revised is the same artifact, not a second one. This is the
// whole difference between generative UI that updates in front of a person and
// generative UI that stacks copies underneath what they are reading.
func TestArtifactRevisionKeepsOneArtifactAndMovesTheVersion(t *testing.T) {
	store := surface.NewArtifactStore(0)
	first, err := store.Put("dash", "Dashboard", "<p>one</p>")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	second, err := store.Put("dash", "Dashboard", "<p>two</p>")
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if first.Version != 1 || second.Version != 2 {
		t.Fatalf("versions must advance, got %d then %d", first.Version, second.Version)
	}
	if listed := store.List(); len(listed) != 1 {
		t.Fatalf("a revision must not add an artifact, got %d", len(listed))
	}
	current, _ := store.Get("dash")
	if current.HTML != "<p>two</p>" {
		t.Fatalf("the revision must be what is served, got %q", current.HTML)
	}
}

func TestArtifactStoreRefusesWhatCannotBeDisplayed(t *testing.T) {
	store := surface.NewArtifactStore(64)
	for name, attempt := range map[string]func() error{
		"no id":      func() error { _, err := store.Put("", "t", "<p>x</p>"); return err },
		"no html":    func() error { _, err := store.Put("a", "t", "   "); return err },
		"bad id":     func() error { _, err := store.Put("../etc", "t", "<p>x</p>"); return err },
		"over limit": func() error { _, err := store.Put("a", "t", strings.Repeat("x", 65)); return err },
	} {
		if err := attempt(); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// The artifact route carries its own policy, and that is the reason it is a
// route. A frame from srcdoc would inherit this application's policy, and
// relaxing that far enough to run model-authored inline script would relax it
// for the surface itself.
func TestArtifactRouteDeniesTheNetworkAndAllowsItsOwnScript(t *testing.T) {
	store := surface.NewArtifactStore(0)
	if _, err := store.Put("card", "Card", "<script>1</script>"); err != nil {
		t.Fatalf("put: %v", err)
	}
	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url(), Artifacts: store})

	response, err := http.Get(local.URL + "/artifacts/card")
	if err != nil {
		t.Fatalf("fetch the artifact: %v", err)
	}
	defer response.Body.Close()
	policy := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "default-src 'none'") {
		t.Fatalf("an artifact must be denied the network, got %q", policy)
	}
	if !strings.Contains(policy, "script-src 'unsafe-inline'") {
		t.Fatalf("an artifact's own script must run, got %q", policy)
	}
	if strings.Contains(policy, "connect-src") {
		t.Fatalf("a connect-src would widen what default-src 'none' already closed: %q", policy)
	}
	if version := response.Header.Get("X-Artifact-Version"); version != "1" {
		t.Fatalf("the served version must be stated, got %q", version)
	}

	missing, err := http.Get(local.URL + "/artifacts/absent")
	if err != nil {
		t.Fatalf("fetch a missing artifact: %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing artifact must be 404, got %d", missing.StatusCode)
	}
}

// --- what the surface declares ----------------------------------------------

func TestArtifactChannelIsDeclaredWithNoBrowserAndNoFiles(t *testing.T) {
	host := surface.NewToolHost(nil, nil, nil)
	tool, declared := host.Lookup("display_artifact")
	if !declared {
		t.Fatal("generative UI must not depend on a browser or a disk")
	}
	if tool.Channel != surface.ChannelArtifact {
		t.Fatalf("display_artifact belongs to the artifact channel, got %q", tool.Channel)
	}
	for _, other := range host.Tools() {
		if other.Channel == surface.ChannelComputer {
			t.Fatalf("computer use must not be declared without a target, found %q", other.Name)
		}
	}
}

// A tool declared and not runnable is worse than one absent: the model spends
// a turn on it and learns nothing. So the computer-use vocabulary appears only
// when there is a browser behind it.
func TestComputerUseIsDeclaredOnlyWithABrowser(t *testing.T) {
	host := surface.NewToolHost(fileHost(t), connectFakeBrowser(t), nil)
	var computer, files, artifacts int
	for _, tool := range host.Tools() {
		switch tool.Channel {
		case surface.ChannelComputer:
			computer++
		case surface.ChannelTool:
			files++
		case surface.ChannelArtifact:
			artifacts++
		}
	}
	if computer != 10 {
		t.Fatalf("the published vocabulary is ten actions, got %d", computer)
	}
	if files != 3 || artifacts != 1 {
		t.Fatalf("expected three file tools and one artifact tool, got %d and %d", files, artifacts)
	}
}

// Policy means "inside the declared target", not "ask". Every clicking action
// declares policy; if that meant asking, a developer would answer a dialog per
// keystroke and stop watching the run.
func TestPolicyAdmitsTheDeclaredSourceAndRefusesEveryOther(t *testing.T) {
	browserContext := connectFakeBrowser(t)
	host := surface.NewToolHost(nil, browserContext, nil)
	source := browserContext.Source()

	if !host.Admits("computer.click", json.RawMessage(`{"source":"`+source+`","x":10,"y":10}`)) {
		t.Fatal("an action inside the declared target must be admitted")
	}
	for _, elsewhere := range []string{"screen", "camera", "", "browser-2"} {
		arguments := json.RawMessage(`{"source":"` + elsewhere + `","x":10,"y":10}`)
		if host.Admits("computer.click", arguments) {
			t.Fatalf("an action on source %q must not be admitted by this target", elsewhere)
		}
	}
}

func TestConfigNamesEveryChannelTheSurfaceCanCarry(t *testing.T) {
	browserContext := connectFakeBrowser(t)
	local := startSurface(t, surface.Config{
		Endpoint: newEndpoint(t).url(), Files: fileHost(t), Browser: browserContext, Token: "secret",
	})
	response, err := http.Get(local.URL + "/api/config")
	if err != nil {
		t.Fatalf("fetch the configuration: %v", err)
	}
	defer response.Body.Close()

	var described struct {
		Authenticated bool `json:"authenticated"`
		Browser       struct {
			Available bool   `json:"available"`
			Source    string `json:"source"`
			Width     int    `json:"width"`
		} `json:"browser"`
		Tools []struct {
			Name    string `json:"name"`
			Channel string `json:"channel"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(response.Body).Decode(&described); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !described.Authenticated {
		t.Fatal("the page must be told a credential is held, and never the credential")
	}
	if body, _ := json.Marshal(described); strings.Contains(string(body), "secret") {
		t.Fatal("the credential must not reach the page")
	}
	if !described.Browser.Available || described.Browser.Source == "" || described.Browser.Width == 0 {
		t.Fatalf("the browser channel must describe itself, got %+v", described.Browser)
	}
	channels := map[string]bool{}
	for _, tool := range described.Tools {
		channels[tool.Channel] = true
	}
	for _, expected := range []string{"tool", "computer", "artifact"} {
		if !channels[expected] {
			t.Fatalf("the page must be told which channel each tool is on; %q is missing", expected)
		}
	}
}

// --- the relay --------------------------------------------------------------

func TestSessionRelayAttachesTheCredentialAndCarriesTheBytes(t *testing.T) {
	remote := newEndpoint(t)
	local := startSurface(t, surface.Config{Endpoint: remote.url(), Token: "token-value"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(local.URL, "http")+"/api/session", nil)
	if err != nil {
		t.Fatalf("dial the surface: %v", err)
	}
	defer page.CloseNow()

	if authorization := <-remote.token; authorization != "Bearer token-value" {
		t.Fatalf("the surface must attach the credential a browser cannot, got %q", authorization)
	}
	if err := page.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if arrived := <-remote.got; arrived != `{"type":"session.update"}` {
		t.Fatalf("the relay must not alter what the page sent, got %q", arrived)
	}
	remote.send <- `{"type":"session.created"}`
	_, payload, err := page.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(payload) != `{"type":"session.created"}` {
		t.Fatalf("the relay must not alter what the server sent, got %q", payload)
	}
}

func TestAnUnreachableEndpointIsReportedInTheProtocolsOwnVocabulary(t *testing.T) {
	local := startSurface(t, surface.Config{
		Endpoint: "ws://127.0.0.1:1/v1/realtime", DialTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	page, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(local.URL, "http")+"/api/session", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer page.CloseNow()
	_, payload, err := page.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var failure struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &failure) != nil || failure.Type != "error" {
		t.Fatalf("one error path in the client must cover a server that is not there, got %q", payload)
	}
	if failure.Error.Code != "endpoint_unreachable" {
		t.Fatalf("the reason must be nameable, got %q", failure.Error.Code)
	}
}

func TestTheSurfaceRefusesToListenWhereItCouldBeReached(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8768", "192.168.1.4:8768"} {
		if err := surface.LoopbackOnly(address); err == nil {
			t.Fatalf("%s runs tools and a browser; it must be refused", address)
		}
	}
	for _, address := range []string{"127.0.0.1:8768", "localhost:8768", "[::1]:8768"} {
		if err := surface.LoopbackOnly(address); err != nil {
			t.Fatalf("%s is loopback and must be allowed: %v", address, err)
		}
	}
}

// --- the browser channel ----------------------------------------------------

// The frame the page displays and the coordinate space actions are expressed
// in arrive together, because a coordinate without the space the model saw is
// the one computer-use failure that produces a plausible click on the wrong
// thing.
func TestBrowserFrameCarriesTheGeometryItWasCapturedIn(t *testing.T) {
	fake := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	browserContext, err := surface.ConnectBrowser(ctx, surface.BrowserConfig{DevToolsURL: fake.server.URL})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer browserContext.Close()

	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url(), Browser: browserContext})
	fetch := func() map[string]any {
		response, err := http.Get(local.URL + "/api/browser/frame")
		if err != nil {
			t.Fatalf("fetch a frame: %v", err)
		}
		defer response.Body.Close()
		var captured map[string]any
		if err := json.NewDecoder(response.Body).Decode(&captured); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return captured
	}

	first := fetch()
	if first["width"].(float64) != 1280 || first["height"].(float64) != 720 {
		t.Fatalf("the declared geometry must be the captured one, got %v×%v", first["width"], first["height"])
	}
	decoded, err := base64.StdEncoding.DecodeString(first["frame"].(string))
	if err != nil || string(decoded) != "jpeg-bytes" {
		t.Fatalf("the frame must be the browser's own bytes, got %q (%v)", decoded, err)
	}
	if first["url"] != "https://example.test/start" {
		t.Fatalf("the page's location travels with the frame, got %v", first["url"])
	}

	// A page that resized is a page whose coordinates changed, and a space the
	// client believes and the browser has left behind is exactly the failure
	// re-reading the viewport every capture prevents.
	fake.resize(800, 600)
	second := fetch()
	if second["width"].(float64) != 800 || second["height"].(float64) != 600 {
		t.Fatalf("a resize must move the declared geometry, got %v×%v", second["width"], second["height"])
	}
}

func TestAnActionReachesTheBrowserAndAnotherSourceDoesNot(t *testing.T) {
	fake := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	browserContext, err := surface.ConnectBrowser(ctx, surface.BrowserConfig{DevToolsURL: fake.server.URL})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer browserContext.Close()

	source := browserContext.Source()
	if _, err := browserContext.Act(ctx, "call-1", "computer.click",
		json.RawMessage(`{"source":"`+source+`","x":120,"y":64}`)); err != nil {
		t.Fatalf("a click inside the declared target must reach the browser: %v", err)
	}
	pressed := fake.performed("Input.dispatchMouseEvent")
	if len(pressed) == 0 {
		t.Fatal("the browser was never told to click")
	}

	if _, err := browserContext.Act(ctx, "call-2", "computer.click",
		json.RawMessage(`{"source":"camera","x":10,"y":10}`)); err == nil {
		t.Fatal("an action on the camera must be refused: you observe it, you do not act on it")
	}
	// An action outside the space the model saw is not a near miss - it is an
	// action on something nobody looked at.
	if _, err := browserContext.Act(ctx, "call-3", "computer.click",
		json.RawMessage(`{"source":"`+source+`","x":99999,"y":10}`)); err == nil {
		t.Fatal("a coordinate outside the declared space must be refused")
	}
}

// Models write fragments. Asked for a table, a good one returns a style block
// and a table and stops, because that is what the answer is - and this was
// found by a real model doing exactly that against the live run.
func TestAFragmentIsWrappedAndADocumentIsLeftAlone(t *testing.T) {
	store := surface.NewArtifactStore(0)
	fragment, err := store.Put("table", "Notes", `<style>td{padding:8px}</style><table><tr><td>x</td></tr></table>`)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	document := fragment.Document()
	for _, required := range []string{"<!doctype html>", `charset="utf-8"`, "width=device-width", "<table>"} {
		if !strings.Contains(document, required) {
			t.Fatalf("a wrapped fragment must carry %q, got %q", required, document)
		}
	}
	// What the model wrote stays inspectable. What it produced and what the
	// browser loads are different questions, and a developer looking at an
	// artifact that came out wrong needs to be able to ask the first one.
	if strings.Contains(fragment.HTML, "<!doctype") {
		t.Fatalf("the model's own bytes must not be rewritten, got %q", fragment.HTML)
	}

	whole, err := store.Put("page", "Page", "<!DOCTYPE html><html><body>done</body></html>")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if whole.Document() != whole.HTML {
		t.Fatalf("a complete document must be served unchanged, got %q", whole.Document())
	}
}

// A title is a model's own words, and one containing markup would otherwise be
// writing markup into the head of the document it names.
func TestATitleCannotEscapeTheTagItIsIn(t *testing.T) {
	store := surface.NewArtifactStore(0)
	artifact, err := store.Put("x", `</title><script>alert(1)</script>`, "<p>body</p>")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	document := artifact.Document()
	if strings.Contains(document, "<script>alert(1)</script>") {
		t.Fatalf("a title must not close its own tag, got %q", document)
	}
	if !strings.Contains(document, "&lt;/title&gt;") {
		t.Fatalf("the title must be escaped rather than dropped, got %q", document)
	}
}

// A resize moves the fence, not just the label.
//
// The dispatcher validates a coordinate against the target it was built with,
// so a declaration that moved and a dispatcher that did not is a page whose
// actions are checked against a screen that is no longer there - admitting
// clicks off the bottom of a shrunk window, and refusing legitimate ones on a
// grown one.
func TestAResizeMovesWhatAnActionIsCheckedAgainst(t *testing.T) {
	fake := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := surface.ConnectBrowser(ctx, surface.BrowserConfig{DevToolsURL: fake.server.URL})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer page.Close()
	source := page.Source()
	at := func(y int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"source":%q,"x":10,"y":%d}`, source, y))
	}

	if _, err := page.Act(ctx, "call-1", "computer.click", at(700)); err != nil {
		t.Fatalf("y=700 is inside 1280x720 and must be admitted: %v", err)
	}

	fake.resize(800, 600)
	if _, _, _, err := page.CaptureFrame(ctx); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if width, height := page.Viewport(); width != 800 || height != 600 {
		t.Fatalf("the declaration must follow the page, got %d×%d", width, height)
	}
	if _, err := page.Act(ctx, "call-2", "computer.click", at(700)); err == nil {
		t.Fatal("y=700 is off the bottom of 800x600 and must now be refused")
	}
	if _, err := page.Act(ctx, "call-3", "computer.click", at(500)); err != nil {
		t.Fatalf("y=500 is inside 800x600 and must still be admitted: %v", err)
	}
}
