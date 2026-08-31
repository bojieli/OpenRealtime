package console_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/console"
	"github.com/coder/websocket"
)

// endpoint is a stand-in OpenRealtime server. It records what reached it and
// emits what a test scripts, so the console has to be indistinguishable from
// any other client to it.
type endpoint struct {
	server *httptest.Server
	token  chan string
	query  chan string
	got    chan string
	send   chan string
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	fake := &endpoint{
		token: make(chan string, 4), query: make(chan string, 4),
		got: make(chan string, 64), send: make(chan string, 64),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fake.token <- request.Header.Get("Authorization")
		fake.query <- request.URL.RawQuery
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
				if err := connection.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *endpoint) url() string {
	return "ws" + strings.TrimPrefix(fake.server.URL, "http")
}

func startConsole(t *testing.T, config console.Config) *httptest.Server {
	t.Helper()
	server, err := console.New(config)
	if err != nil {
		t.Fatalf("new console: %v", err)
	}
	local := httptest.NewServer(server.Handler())
	t.Cleanup(local.Close)
	return local
}

func dialConsole(t *testing.T, local *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(local.URL, "http") + path
	connection, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{local.URL}},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { connection.CloseNow() })
	connection.SetReadLimit(16 << 20)
	return connection
}

func readMessage(t *testing.T, connection *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode %q: %v", payload, err)
	}
	return decoded
}

func writeMessage(t *testing.T, connection *websocket.Conn, message any) {
	t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func expect[T any](t *testing.T, channel chan T, what string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// The page is the protocol client. The console carries its bytes and attaches
// the credential a browser cannot set, and changes nothing else.
func TestTheSessionIsRelayedVerbatimWithTheCredentialAttached(t *testing.T) {
	t.Parallel()
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{
		Endpoint: fake.url(), Token: "sekrit", Model: "openrealtime",
	})
	page := dialConsole(t, local, "/api/session")

	if header := expect(t, fake.token, "the upgrade request"); header != "Bearer sekrit" {
		t.Fatalf("the console must present the credential: %q", header)
	}
	if query := expect(t, fake.query, "the query"); !strings.Contains(query, "model=openrealtime") {
		t.Fatalf("the model must reach the endpoint: %q", query)
	}

	// A frame is the largest thing that crosses this hop, and the relay has to
	// carry one: the library default would drop the connection at 32 KiB.
	big := strings.Repeat("Q", 900_000)
	writeMessage(t, page, map[string]any{
		"type": "openrealtime.input_video_frame.append", "source": "screen", "frame": big,
	})
	arrived := expect(t, fake.got, "the frame")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arrived), &decoded); err != nil {
		t.Fatalf("the frame did not arrive as JSON: %v", err)
	}
	if decoded["frame"] != big {
		t.Fatalf("the frame arrived with %d of %d characters",
			len(decoded["frame"].(string)), len(big))
	}

	fake.send <- `{"type":"session.created","session":{"id":"sess_1"}}`
	if event := readMessage(t, page); event["type"] != "session.created" {
		t.Fatalf("the reply must reach the page unchanged: %v", event)
	}
}

// A server that is not there should look like a protocol error rather than a
// socket that closed, so the page needs one error path rather than two.
func TestAnUnreachableEndpointIsReportedAsAProtocolError(t *testing.T) {
	t.Parallel()
	local := startConsole(t, console.Config{
		Endpoint: "ws://127.0.0.1:1/v1/realtime", DialTimeout: 2 * time.Second,
	})
	page := dialConsole(t, local, "/api/session")
	event := readMessage(t, page)
	if event["type"] != "error" {
		t.Fatalf("expected an error event, got %v", event)
	}
	message := event["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "could not reach") {
		t.Fatalf("the error must say what failed: %q", message)
	}
}

func TestTheConfigDescribesWhatThisConsoleCanDo(t *testing.T) {
	t.Parallel()
	host, _ := newHost(t, "read_file", "write_file")
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{
		Endpoint: fake.url(), Host: host, Token: "sekrit",
		WebRTCEndpoint: "http://127.0.0.1:8766/v1/realtime/calls",
	})
	response, err := http.Get(local.URL + "/api/config")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	defer response.Body.Close()
	var described struct {
		WebRTC        bool   `json:"webrtc"`
		Authenticated bool   `json:"authenticated"`
		Root          string `json:"root"`
		Tools         []struct {
			Name       string          `json:"name"`
			Confirm    string          `json:"confirm"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(response.Body).Decode(&described); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !described.WebRTC || !described.Authenticated || described.Root == "" {
		t.Fatalf("the page needs to know what is available: %+v", described)
	}
	if len(described.Tools) != 2 {
		t.Fatalf("expected two tools, got %d", len(described.Tools))
	}
	// The credential is the one thing that must never reach the page.
	for _, tool := range described.Tools {
		if tool.Name == "write_file" && tool.Confirm != "always" {
			t.Fatalf("the confirmation requirement must travel with the declaration: %+v", tool)
		}
	}
}

func TestTheCredentialNeverReachesThePage(t *testing.T) {
	t.Parallel()
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url(), Token: "sekrit-value"})
	for _, path := range []string{"/api/config", "/", "/app.js", "/transport.js"} {
		response, err := http.Get(local.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body := make([]byte, 1<<20)
		n, _ := response.Body.Read(body)
		response.Body.Close()
		if strings.Contains(string(body[:n]), "sekrit-value") {
			t.Fatalf("%s leaked the credential to the browser", path)
		}
	}
}

// --- the confirmation gate --------------------------------------------------

func callTool(t *testing.T, socket *websocket.Conn, id, name string, arguments any) {
	t.Helper()
	encoded, _ := json.Marshal(arguments)
	writeMessage(t, socket, map[string]any{
		"type": "call", "id": id, "name": name, "arguments": json.RawMessage(encoded),
	})
}

// The decision happens before the effect, and it happens in this process. A
// page that never asked, or that was talked into not asking, still cannot make
// anything happen.
func TestAToolThatNeedsConfirmingDoesNothingUntilSomeoneAgrees(t *testing.T) {
	t.Parallel()
	host, root := newHost(t, "write_file")
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url(), Host: host})
	socket := dialConsole(t, local, "/api/tools")

	target := filepath.Join(root, "written.txt")
	callTool(t, socket, "call_1", "write_file", map[string]any{
		"path": "written.txt", "content": "from the model",
	})

	request := readMessage(t, socket)
	if request["type"] != "confirm" || request["id"] != "call_1" {
		t.Fatalf("expected a confirmation request, got %v", request)
	}
	if request["confirm"] != "always" {
		t.Fatalf("the request must carry the declared requirement: %v", request)
	}
	// Nothing has happened yet, and that is the entire point.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("the file was written before anyone agreed to it")
	}

	writeMessage(t, socket, map[string]any{"type": "decide", "id": "call_1", "approved": true})
	result := readMessage(t, socket)
	if result["type"] != "result" || result["error"] != nil {
		t.Fatalf("expected a result, got %v", result)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "from the model" {
		t.Fatalf("the approved write did not happen: %q %v", content, err)
	}
}

func TestDecliningRefusesTheActionAndSaysSo(t *testing.T) {
	t.Parallel()
	host, root := newHost(t, "write_file")
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url(), Host: host})
	socket := dialConsole(t, local, "/api/tools")

	callTool(t, socket, "call_1", "write_file", map[string]any{"path": "no.txt", "content": "x"})
	if request := readMessage(t, socket); request["type"] != "confirm" {
		t.Fatalf("expected a confirmation request, got %v", request)
	}
	writeMessage(t, socket, map[string]any{"type": "decide", "id": "call_1", "approved": false})

	result := readMessage(t, socket)
	// A refusal is a result rather than silence: the model has to learn it was
	// refused, or the session waits for something that is never coming.
	if result["type"] != "result" {
		t.Fatalf("expected a result, got %v", result)
	}
	message, _ := result["error"].(string)
	if !strings.Contains(message, "declined") {
		t.Fatalf("a refusal must say it was a refusal: %v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "no.txt")); !os.IsNotExist(err) {
		t.Fatal("a declined action happened anyway")
	}
}

func TestAToolDeclaredWithoutConfirmationRunsStraightAway(t *testing.T) {
	t.Parallel()
	host, root := newHost(t, "read_file")
	if err := os.WriteFile(filepath.Join(root, "open.txt"), []byte("readable"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url(), Host: host})
	socket := dialConsole(t, local, "/api/tools")

	callTool(t, socket, "call_1", "read_file", map[string]any{"path": "open.txt"})
	result := readMessage(t, socket)
	if result["type"] != "result" || result["output"] != "readable" {
		t.Fatalf("a tool with no confirmation requirement should just run: %v", result)
	}
}

func TestAnUndeclaredToolIsRefusedRatherThanRun(t *testing.T) {
	t.Parallel()
	host, _ := newHost(t, "read_file")
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url(), Host: host})
	socket := dialConsole(t, local, "/api/tools")

	callTool(t, socket, "call_1", "run_command", map[string]any{"command": "echo hi"})
	result := readMessage(t, socket)
	message, _ := result["error"].(string)
	if !strings.Contains(message, "no tool named") {
		t.Fatalf("expected a refusal for an undeclared tool, got %v", result)
	}
}

// Two calls in flight at once must not have their confirmations confused,
// because the wrong answer applied to the wrong action is the worst possible
// outcome for a mechanism whose whole job is asking.
func TestConcurrentConfirmationsStayMatchedToTheirCalls(t *testing.T) {
	t.Parallel()
	host, root := newHost(t, "write_file")
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url(), Host: host})
	socket := dialConsole(t, local, "/api/tools")

	callTool(t, socket, "call_a", "write_file", map[string]any{"path": "a.txt", "content": "a"})
	callTool(t, socket, "call_b", "write_file", map[string]any{"path": "b.txt", "content": "b"})

	seen := map[string]bool{}
	for range 2 {
		request := readMessage(t, socket)
		if request["type"] != "confirm" {
			t.Fatalf("expected confirmation requests, got %v", request)
		}
		seen[request["id"].(string)] = true
	}
	if !seen["call_a"] || !seen["call_b"] {
		t.Fatalf("both calls must be asked about: %v", seen)
	}

	// Approve one and decline the other.
	writeMessage(t, socket, map[string]any{"type": "decide", "id": "call_a", "approved": true})
	writeMessage(t, socket, map[string]any{"type": "decide", "id": "call_b", "approved": false})
	for range 2 {
		readMessage(t, socket)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("the approved call did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("the declined call ran anyway")
	}
}

func TestAConsoleWithNoToolsSaysSoRatherThanHanging(t *testing.T) {
	t.Parallel()
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url()})
	socket := dialConsole(t, local, "/api/tools")
	result := readMessage(t, socket)
	if !strings.Contains(result["error"].(string), "no tools") {
		t.Fatalf("expected an explanation, got %v", result)
	}
}

// --- reachability -----------------------------------------------------------

func TestTheConsoleRefusesToListenOffLoopback(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"0.0.0.0:8767", "192.168.1.10:8767", "[::]:8767"} {
		if err := console.LoopbackOnly(address); err == nil {
			t.Fatalf("%s puts a shell on the network and must be refused", address)
		}
	}
	for _, address := range []string{"127.0.0.1:8767", "[::1]:8767", "localhost:8767", ":8767"} {
		if err := console.LoopbackOnly(address); err != nil {
			t.Fatalf("%s is loopback and should be allowed: %v", address, err)
		}
	}
}

func TestTheConsoleRequiresAWebSocketEndpoint(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"", "http://127.0.0.1:8765/v1/realtime", "not a url"} {
		if _, err := console.New(console.Config{Endpoint: endpoint}); err == nil {
			t.Fatalf("%q is not a protocol endpoint and should be refused", endpoint)
		}
	}
}

func TestThePageIsServedWithAPolicyThatMatchesWhatItDoes(t *testing.T) {
	t.Parallel()
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url()})
	response, err := http.Get(local.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	policy := response.Header.Get("Content-Security-Policy")
	// Every script is a file on this origin, so nothing needs 'unsafe-inline'
	// and the policy should not grant it.
	if strings.Contains(policy, "unsafe-inline") {
		t.Fatalf("the console loads no inline script and should not allow one: %q", policy)
	}
	for _, directive := range []string{"default-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("policy is missing %q: %q", directive, policy)
		}
	}
}
