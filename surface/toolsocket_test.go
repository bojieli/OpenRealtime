package surface_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/surface"
	"github.com/coder/websocket"
)

// socket is a page's end of the tool connection.
type socket struct {
	t          *testing.T
	connection *websocket.Conn
	ctx        context.Context
}

func dialTools(t *testing.T, local *httptest.Server) *socket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	connection, _, err := websocket.Dial(
		ctx, "ws"+strings.TrimPrefix(local.URL, "http")+"/api/tools", nil)
	if err != nil {
		t.Fatalf("dial the tool socket: %v", err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	return &socket{t: t, connection: connection, ctx: ctx}
}

func (page *socket) send(message map[string]any) {
	page.t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		page.t.Fatalf("encode: %v", err)
	}
	if err := page.connection.Write(page.ctx, websocket.MessageText, encoded); err != nil {
		page.t.Fatalf("write: %v", err)
	}
}

func (page *socket) receive() map[string]any {
	page.t.Helper()
	_, payload, err := page.connection.Read(page.ctx)
	if err != nil {
		page.t.Fatalf("read: %v", err)
	}
	var message map[string]any
	if err := json.Unmarshal(payload, &message); err != nil {
		page.t.Fatalf("decode %q: %v", payload, err)
	}
	return message
}

func (page *socket) call(id, name string, arguments map[string]any) {
	page.send(map[string]any{"type": "call", "id": id, "name": name, "arguments": arguments})
}

func TestAToolRunsWhereTheFilesAreAndItsOutputComesBack(t *testing.T) {
	files := fileHost(t)
	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url(), Files: files})
	page := dialTools(t, local)

	page.call("call-1", "read_file", map[string]any{"path": "note.txt"})
	result := page.receive()
	if result["type"] != "result" || result["id"] != "call-1" {
		t.Fatalf("expected a result for call-1, got %v", result)
	}
	if error, failed := result["error"]; failed {
		t.Fatalf("reading a seeded file must succeed, got %v", error)
	}
	if !strings.Contains(result["output"].(string), "hello from disk") {
		t.Fatalf("the file's contents must come back, got %v", result["output"])
	}
	if result["channel"] != string(surface.ChannelTool) {
		t.Fatalf("a file tool is on the tool channel, got %v", result["channel"])
	}
}

// The decision is made before the effect, by someone who can see what was
// asked. A test that only checked the dialog appeared would miss the half that
// matters: that nothing has happened on the disk while it is open.
func TestAConfirmingToolChangesNothingUntilAPersonAnswers(t *testing.T) {
	files := fileHost(t)
	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url(), Files: files})
	page := dialTools(t, local)

	target := filepath.Join(files.Root(), "written.txt")
	page.call("call-2", "write_file", map[string]any{"path": "written.txt", "content": "from the model"})

	request := page.receive()
	if request["type"] != "confirm" || request["name"] != "write_file" {
		t.Fatalf("a mutating tool must ask first, got %v", request)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the file exists while the question is still on screen")
	}

	page.send(map[string]any{"type": "decide", "id": "call-2", "approved": true})
	result := page.receive()
	if result["type"] != "result" || result["error"] != nil {
		t.Fatalf("an approved write must succeed, got %v", result)
	}
	written, err := os.ReadFile(target)
	if err != nil || string(written) != "from the model" {
		t.Fatalf("the approved write must have happened, got %q (%v)", written, err)
	}
}

// A refusal is a result, not silence. The model asked, a person said no, and
// the session has to learn that rather than wait for something never coming.
func TestADeclinedToolAnswersTheSession(t *testing.T) {
	files := fileHost(t)
	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url(), Files: files})
	page := dialTools(t, local)

	page.call("call-3", "write_file", map[string]any{"path": "declined.txt", "content": "no"})
	if request := page.receive(); request["type"] != "confirm" {
		t.Fatalf("expected a confirmation, got %v", request)
	}
	page.send(map[string]any{"type": "decide", "id": "call-3", "approved": false})

	result := page.receive()
	if result["type"] != "result" {
		t.Fatalf("a refusal must still be a result, got %v", result)
	}
	if failure, _ := result["error"].(string); !strings.Contains(failure, "declined") {
		t.Fatalf("the model must be told it was declined, got %v", result["error"])
	}
	if _, err := os.Stat(filepath.Join(files.Root(), "declined.txt")); err == nil {
		t.Fatal("a declined write must not have happened")
	}
}

// The page renders the artifact from what the host says it stored, not from
// the model's JSON echoed back. What gets displayed should not depend on a
// model's output being what this host just said it was.
func TestAnArtifactArrivesAlongsideTheResult(t *testing.T) {
	store := surface.NewArtifactStore(0)
	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url(), Artifacts: store})
	page := dialTools(t, local)

	page.call("call-4", "display_artifact", map[string]any{
		"artifact_id": "totals", "title": "Totals",
		"html": "<!doctype html><p>42</p>",
	})
	result := page.receive()
	if result["error"] != nil {
		t.Fatalf("rendering must succeed, got %v", result["error"])
	}
	if result["channel"] != string(surface.ChannelArtifact) {
		t.Fatalf("an artifact is on the artifact channel, got %v", result["channel"])
	}
	artifact, carried := result["artifact"].(map[string]any)
	if !carried {
		t.Fatalf("the page must be handed the artifact, got %v", result)
	}
	if artifact["id"] != "totals" || artifact["version"].(float64) != 1 {
		t.Fatalf("expected totals at version 1, got %v", artifact)
	}
	// The model is told the version so it knows the revision landed.
	var told struct {
		ArtifactID string `json:"artifact_id"`
		Version    int    `json:"version"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal([]byte(result["output"].(string)), &told); err != nil {
		t.Fatalf("the tool output must be JSON the model can read: %v", err)
	}
	if told.ArtifactID != "totals" || told.Version != 1 || told.Status != "displayed" {
		t.Fatalf("the model must learn the revision landed, got %+v", told)
	}
	if stored, exists := store.Get("totals"); !exists || !strings.Contains(stored.HTML, "42") {
		t.Fatalf("the html must be what gets served, got %+v", stored)
	}
}

func TestAnArtifactTooLargeIsRefusedWithSomethingTheModelCanAct(t *testing.T) {
	local := startSurface(t, surface.Config{
		Endpoint: newEndpoint(t).url(), Artifacts: surface.NewArtifactStore(256),
	})
	page := dialTools(t, local)
	page.call("call-5", "display_artifact", map[string]any{
		"artifact_id": "huge", "title": "Huge", "html": strings.Repeat("x", 512),
	})
	result := page.receive()
	failure, _ := result["error"].(string)
	if !strings.Contains(failure, "512") || !strings.Contains(failure, "256") {
		t.Fatalf("a refusal must say what was sent and what the limit is, got %q", failure)
	}
}

// Policy resolves against the declared target rather than asking, so a run can
// be watched instead of clicked through. An action naming a source this
// context does not own is the case where a person genuinely should be looking.
func TestAnActionInsideTheTargetRunsAndOneOutsideItAsks(t *testing.T) {
	browserContext := connectFakeBrowser(t)
	local := startSurface(t, surface.Config{
		Endpoint: newEndpoint(t).url(), Browser: browserContext,
	})
	page := dialTools(t, local)
	source := browserContext.Source()

	page.call("call-6", "computer.click", map[string]any{"source": source, "x": 40, "y": 40})
	result := page.receive()
	if result["type"] != "result" {
		t.Fatalf("an action inside the target must not stop to ask, got %v", result)
	}
	if result["error"] != nil {
		t.Fatalf("the click must have reached the browser, got %v", result["error"])
	}
	if result["channel"] != string(surface.ChannelComputer) {
		t.Fatalf("an action is on the computer channel, got %v", result["channel"])
	}
	// The output stays text. No image is ever carried in a function result.
	if strings.Contains(result["output"].(string), "base64") {
		t.Fatalf("an action result must stay text, got %v", result["output"])
	}

	page.call("call-7", "computer.click", map[string]any{"source": "screen", "x": 40, "y": 40})
	asked := page.receive()
	if asked["type"] != "confirm" {
		t.Fatalf("an action on an unowned source must reach a person, got %v", asked)
	}
	page.send(map[string]any{"type": "decide", "id": "call-7", "approved": true})
	refused := page.receive()
	// Approving it does not make it legal: the dispatcher's fence is checked
	// again at the point of effect, which is where it has to be.
	if refused["error"] == nil {
		t.Fatalf("approval must not widen the declared target, got %v", refused)
	}
}

func TestAnUndeclaredToolIsRefusedRatherThanIgnored(t *testing.T) {
	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url()})
	page := dialTools(t, local)
	page.call("call-8", "rm_rf", map[string]any{})
	result := page.receive()
	if failure, _ := result["error"].(string); !strings.Contains(failure, "rm_rf") {
		t.Fatalf("an undeclared tool must be named in the refusal, got %v", result)
	}
}
