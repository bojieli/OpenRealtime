package surface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/coder/websocket"
)

// The tool socket carries four message types, and the shape of the exchange is
// the point of it.
//
// A call that needs confirming is not executed and then reported. It is held
// here, a request goes to the page, and nothing happens until a person
// answers - so the decision is made before the effect, and it is made by
// someone who can see what was asked. The page renders the dialog because that
// is where the person is looking; this process holds the veto because that is
// what touches the disk and drives the browser. Trusting the page to have
// asked would make confirmation a property of a script the session can
// influence.
const (
	messageCall    = "call"
	messageDecide  = "decide"
	messageConfirm = "confirm"
	messageResult  = "result"
)

type toolMessage struct {
	Type string `json:"type"`
	// ID correlates a call with its confirmation and its result. It is the
	// protocol's call identifier, so the page can match a result to the tool
	// call the session is waiting on.
	ID        string          `json:"id"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Approved answers a confirmation request.
	Approved bool `json:"approved,omitempty"`
	// Confirm is the declared requirement, echoed so the page can say why it
	// is asking.
	Confirm string  `json:"confirm,omitempty"`
	Channel Channel `json:"channel,omitempty"`
	Output  string  `json:"output,omitempty"`
	Error   string  `json:"error,omitempty"`
	// Artifact is set when this call rendered generative UI. The page could
	// parse the output instead, but then what gets displayed would depend on a
	// model's JSON being what this host just said it was, which is a
	// dependency with no upside.
	Artifact *Artifact `json:"artifact,omitempty"`
	Download *Download `json:"download,omitempty"`
}

type pendingDecision struct {
	answer chan bool
	once   sync.Once
}

func (pending *pendingDecision) resolve(approved bool) {
	pending.once.Do(func() { pending.answer <- approved })
}

type toolSession struct {
	host       *ToolHost
	logger     *slog.Logger
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
				Type: messageResult, Error: "the surface received a malformed tool message",
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
	tool, declared := session.host.Lookup(call.Name)
	if !declared {
		_ = session.write(ctx, toolMessage{
			Type: messageResult, ID: call.ID,
			Error: fmt.Sprintf("no tool named %q is declared by this surface", call.Name),
		})
		return
	}

	if session.needsAsking(tool, call.Arguments) {
		approved, err := session.confirm(ctx, call, tool)
		if err != nil {
			return
		}
		if !approved {
			// A refusal is a result, not a failure. The model asked, a person
			// said no, and the session has to learn that rather than wait.
			_ = session.write(ctx, toolMessage{
				Type: messageResult, ID: call.ID, Channel: tool.Channel,
				Error: "the developer declined this action",
			})
			return
		}
	}

	outcome, err := session.host.Run(ctx, call.ID, call.Name, call.Arguments)
	message := toolMessage{Type: messageResult, ID: call.ID, Channel: tool.Channel}
	if err != nil {
		message.Error = err.Error()
	} else {
		message.Output = outcome.Output
		message.Artifact = outcome.Artifact
		message.Download = outcome.Download
	}
	if writeErr := session.write(ctx, message); writeErr != nil {
		session.logger.Warn("tool result could not be delivered",
			"tool", call.Name, "error", writeErr)
	}
}

// needsAsking decides whether a person has to answer before this runs.
//
// "never" runs. "always" asks. "policy" is resolved against the declared
// target first and only asks when the target does not admit it - which for a
// computer-use action means the action named a source this context does not
// own, and is the case where a person genuinely should be looking.
func (session *toolSession) needsAsking(tool Tool, arguments json.RawMessage) bool {
	switch action.Confirm(tool.Confirm) {
	case action.ConfirmNever, "":
		return false
	case action.ConfirmPolicy:
		return !session.host.Admits(tool.Name, arguments)
	default:
		return true
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
		Arguments: call.Arguments, Confirm: tool.Confirm, Channel: tool.Channel,
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
