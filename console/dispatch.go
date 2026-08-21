package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// The tool socket carries three message types, and the shape of the exchange
// is the point of it.
//
// A call that needs confirming is not executed and then reported. It is held
// here, a request goes to the page, and nothing happens until a person answers
// - so the decision is made before the effect, and it is made by someone who
// can see what was asked. The page renders the dialog because that is where
// the person is looking; this process holds the veto because that is what
// touches the disk. Trusting the page to have asked would make confirmation a
// property of a script the session can influence.
const (
	messageCall    = "call"
	messageDecide  = "decide"
	messageConfirm = "confirm"
	messageResult  = "result"
)

type toolMessage struct {
	Type string `json:"type"`
	// ID correlates a call with its confirmation and its result. It is the
	// protocol's call identifier, so a page can match a result to the tool
	// call the session is waiting on.
	ID        string          `json:"id"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Approved answers a confirmation request.
	Approved bool `json:"approved,omitempty"`
	// Confirm is the declared requirement, echoed so the page can say why it
	// is asking.
	Confirm string `json:"confirm,omitempty"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
}

type pendingDecision struct {
	answer chan bool
	once   sync.Once
}

func (pending *pendingDecision) resolve(approved bool) {
	pending.once.Do(func() { pending.answer <- approved })
}

// tools serves one page's tool socket.
func (server *Server) tools(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	connection.SetReadLimit(8 << 20)
	defer connection.CloseNow()

	if server.config.Host == nil {
		_ = writeTool(request.Context(), connection, toolMessage{
			Type: messageResult, Error: "this console was started with no tools",
		})
		return
	}

	session := &toolSession{
		server: server, connection: connection,
		decisions: make(map[string]*pendingDecision),
	}
	session.serve(request.Context())
}

type toolSession struct {
	server     *Server
	connection *websocket.Conn

	mu sync.Mutex
	// writeMu serialises writes, because a tool that finishes while another is
	// waiting to be confirmed would otherwise interleave two frames.
	writeMu   sync.Mutex
	decisions map[string]*pendingDecision
	running   sync.WaitGroup
}

func (session *toolSession) serve(ctx context.Context) {
	// A tool that is still running when the page goes away is left to finish
	// rather than cancelled: it has already touched the world, and abandoning
	// it half way would be worse than completing it and discarding the answer.
	defer session.running.Wait()
	for {
		_, payload, err := session.connection.Read(ctx)
		if err != nil {
			session.abandonAll()
			return
		}
		var message toolMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			_ = session.write(ctx, toolMessage{
				Type: messageResult, Error: "the console received a malformed tool message",
			})
			continue
		}
		switch message.Type {
		case messageCall:
			session.running.Add(1)
			go func() {
				defer session.running.Done()
				session.dispatch(ctx, message)
			}()
		case messageDecide:
			session.decide(message.ID, message.Approved)
		default:
			_ = session.write(ctx, toolMessage{
				Type: messageResult, ID: message.ID,
				Error: fmt.Sprintf("unknown tool message %q", message.Type),
			})
		}
	}
}

func (session *toolSession) dispatch(ctx context.Context, call toolMessage) {
	host := session.server.config.Host
	tool, declared := host.Lookup(call.Name)
	if !declared {
		_ = session.write(ctx, toolMessage{
			Type: messageResult, ID: call.ID,
			Error: fmt.Sprintf("no tool named %q is declared by this console", call.Name),
		})
		return
	}

	if tool.Confirm != "never" {
		approved, err := session.confirm(ctx, call, tool)
		if err != nil {
			return
		}
		if !approved {
			// A refusal is a result, not a failure. The model asked, a person
			// said no, and the session has to learn that rather than wait.
			_ = session.write(ctx, toolMessage{
				Type: messageResult, ID: call.ID,
				Error: "the developer declined this action",
			})
			return
		}
	}

	output, err := host.Run(ctx, call.Name, call.Arguments)
	result := toolMessage{Type: messageResult, ID: call.ID}
	if err != nil {
		result.Error = err.Error()
	} else {
		result.Output = output
	}
	if writeErr := session.write(ctx, result); writeErr != nil {
		session.server.config.Logger.Warn("tool result could not be delivered",
			"tool", call.Name, "error", writeErr)
	}
}

func (session *toolSession) confirm(ctx context.Context, call toolMessage, tool Tool) (bool, error) {
	pending := &pendingDecision{answer: make(chan bool, 1)}
	session.mu.Lock()
	if _, duplicate := session.decisions[call.ID]; duplicate {
		session.mu.Unlock()
		return false, errors.New("duplicate confirmation")
	}
	session.decisions[call.ID] = pending
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		delete(session.decisions, call.ID)
		session.mu.Unlock()
	}()

	if err := session.write(ctx, toolMessage{
		Type: messageConfirm, ID: call.ID, Name: call.Name,
		Arguments: call.Arguments, Confirm: tool.Confirm,
	}); err != nil {
		return false, err
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case approved := <-pending.answer:
		return approved, nil
	}
}

func (session *toolSession) decide(id string, approved bool) {
	session.mu.Lock()
	pending := session.decisions[id]
	session.mu.Unlock()
	if pending != nil {
		pending.resolve(approved)
	}
}

// abandonAll refuses every outstanding confirmation when the page disconnects.
//
// Nobody is going to answer, and a goroutine waiting forever on a person who
// closed the tab is a leak that looks like a hang.
func (session *toolSession) abandonAll() {
	session.mu.Lock()
	pending := make([]*pendingDecision, 0, len(session.decisions))
	for _, decision := range session.decisions {
		pending = append(pending, decision)
	}
	session.mu.Unlock()
	for _, decision := range pending {
		decision.resolve(false)
	}
}

func (session *toolSession) write(ctx context.Context, message toolMessage) error {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	return writeTool(ctx, session.connection, message)
}

func writeTool(ctx context.Context, connection *websocket.Conn, message toolMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, encoded)
}

// ToolNames is every tool this build knows how to run, for the CLI's help.
func ToolNames() []string {
	all := allTools()
	names := make([]string, 0, len(all))
	for _, tool := range all {
		names = append(names, tool.Name)
	}
	return names
}

// ParseToolSelection turns a comma-separated flag into a tool list.
func ParseToolSelection(value string) []string {
	var selected []string
	for _, name := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			selected = append(selected, trimmed)
		}
	}
	return selected
}
