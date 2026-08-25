package surface

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/console"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Channel is which action channel a tool belongs to.
//
// The page groups what the agent does by channel rather than by tool name,
// because the question a developer is asking while watching a run is "did it
// act, or did it just talk" rather than "which of eleven functions fired". The
// grouping is declared here, next to the thing that runs, rather than inferred
// in the page from a name prefix - an inference in the client and a fact in
// the host are two statements that can disagree, and a bench whose display can
// disagree with what happened is not a bench.
type Channel string

const (
	// ChannelTool is an ordinary function call with a text result.
	ChannelTool Channel = "tool"
	// ChannelComputer is an action on a declared video source.
	ChannelComputer Channel = "computer"
	// ChannelArtifact is HTML rendered for a person to look at.
	ChannelArtifact Channel = "artifact"
)

// Tool is one declared, runnable tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Confirm is the requirement declared to the session: never, policy, or
	// always. It is also enforced here, because the browser is a user
	// interface and this process is what touches the world.
	Confirm string `json:"confirm"`
	// Mutating marks a tool that can change something outside this process.
	Mutating bool    `json:"mutating"`
	Channel  Channel `json:"channel"`

	run func(context.Context, string, json.RawMessage) (result, error)
}

// result is what one tool returns: the text the session receives, and
// whatever the page needs in order to show what happened.
type result struct {
	Output string
	// Artifact is set when this call rendered generative UI, so the page can
	// display it without parsing the tool output back out of a string it just
	// handed to the model.
	Artifact *Artifact
}

// ToolHost declares and runs everything on the action side that is not speech
// or text.
//
// Three channels, three owners: files on this disk, a browser this process
// drives, and HTML held in memory for a frame to display. They are one host
// because they are one declaration - the session is told about all of them in
// a single session.update, and a tool the page knows about and the host does
// not is exactly the disagreement this arrangement exists to prevent.
type ToolHost struct {
	files     *console.Host
	browser   *BrowserContext
	artifacts *ArtifactStore
	policy    action.PolicyDecision
	tools     []Tool
}

// NewToolHost assembles the declared set. Any of the three may be nil, and the
// corresponding channel is then simply absent rather than declared and broken.
func NewToolHost(files *console.Host, browserContext *BrowserContext, artifacts *ArtifactStore) *ToolHost {
	if artifacts == nil {
		artifacts = NewArtifactStore(0)
	}
	host := &ToolHost{files: files, browser: browserContext, artifacts: artifacts}
	if browserContext != nil {
		host.policy = computeruse.TargetPolicy(browserContext.Target())
	}

	if files != nil {
		for _, tool := range files.Tools() {
			name := tool.Name
			host.tools = append(host.tools, Tool{
				Name: name, Description: tool.Description, Parameters: tool.Parameters,
				Confirm: tool.Confirm, Mutating: tool.Mutating, Channel: ChannelTool,
				run: func(ctx context.Context, _ string, arguments json.RawMessage) (result, error) {
					output, err := files.Run(ctx, name, arguments)
					return result{Output: output}, err
				},
			})
		}
	}

	host.tools = append(host.tools, host.artifactTool())

	if browserContext != nil {
		// The narrowed vocabulary, not the target-free one: the model is told
		// which sources exist rather than asked to recover the name from
		// prose. A live run against a real model produced source "video
		// browser" - the observer name and the source name run together,
		// taken from an observation that had honestly reported both - and an
		// enum is what makes that impossible rather than merely unlikely.
		declared, err := computeruse.DefinitionsFor(browserContext.Target())
		if err != nil {
			// A target that will not validate is a browser context that
			// should not have been built, and the constructor already refuses
			// one. Declaring the un-narrowed vocabulary here would hand the
			// model back the guess this exists to remove.
			declared = nil
		}
		for _, definition := range declared {
			name := definition.Name
			host.tools = append(host.tools, Tool{
				Name: name, Description: definition.Description, Parameters: definition.Parameters,
				Confirm: string(definition.DefaultConfirm),
				// Every action in the namespace changes something the person
				// can see, including the ones that only move the pointer.
				Mutating: definition.DefaultConfirm != action.ConfirmNever,
				Channel:  ChannelComputer,
				run: func(ctx context.Context, callID string, arguments json.RawMessage) (result, error) {
					output, err := browserContext.Act(ctx, callID, name, arguments)
					return result{Output: output}, err
				},
			})
		}
	}
	return host
}

// Tools is the declared set, in a stable order.
func (host *ToolHost) Tools() []Tool { return host.tools }

// Lookup finds a declared tool.
func (host *ToolHost) Lookup(name string) (Tool, bool) {
	for _, tool := range host.tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// Artifacts is the store, for a caller serving or inspecting them.
func (host *ToolHost) Artifacts() *ArtifactStore { return host.artifacts }

// Run executes one declared tool.
//
// Confirmation is not checked here: it is the caller's, because the decision
// is a person's and this is what happens after they made it.
func (host *ToolHost) Run(
	ctx context.Context, callID, name string, arguments json.RawMessage,
) (result, error) {
	tool, declared := host.Lookup(name)
	if !declared {
		return result{}, fmt.Errorf("no tool named %q is declared by this surface", name)
	}
	return tool.run(ctx, callID, arguments)
}

// Admits answers a "policy" confirmation requirement without asking a person.
//
// This is what policy means, and it is worth being exact about it because the
// alternative reading makes computer use unusable. Every clicking and typing
// action declares "policy"; if that meant "ask", a developer watching a run
// would answer a dialog for every keystroke the agent typed, and would stop
// watching the run. The fence that bounds the blast radius is the declared
// target - the action names a video source, the target owns exactly one, and
// the dispatcher refuses anything outside it - so policy admits an action that
// lands inside the declared context and refuses everything else, and a
// deployment that wants a person in the loop declares "always" instead.
func (host *ToolHost) Admits(name string, arguments json.RawMessage) bool {
	if host.policy == nil {
		return false
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	return host.policy(trajectory.ToolCall{Name: name, Arguments: arguments})
}

// artifactTool declares generative UI.
//
// It is an ordinary function tool and that is the entire design. There is no
// artifact event, no artifact namespace, and nothing in the protocol that
// knows this exists - a model asks for HTML to be displayed the same way it
// asks for a file to be read, and any Realtime server that can call a function
// can drive it. Rendering is a property of this client, which is the correct
// place for it: what a surface does with a tool's output is the surface's
// business, and the wire stays the size it was.
func (host *ToolHost) artifactTool() Tool {
	parameters := fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "artifact_id": {
      "type": "string",
      "description": "A stable identifier. Calling again with the same id replaces what the person is already looking at, which is how an artifact is revised rather than duplicated. Letters, digits, dash, and underscore, up to 64 characters."
    },
    "title": {
      "type": "string",
      "description": "A short name for this artifact, shown above it."
    },
    "html": {
      "type": "string",
      "description": "The markup to display: either a complete HTML document or just the fragment that is the answer, such as a table or a chart - a fragment is wrapped in a readable document for you. Inline style and script run; the artifact is sandboxed and cannot reach the network, so everything it needs must be in the markup. Up to %d bytes. To send a value back to the conversation, call window.parent.postMessage({text: \"...\"}, \"*\") - it arrives as a message from the person."
    }
  },
  "required": ["artifact_id", "title", "html"],
  "additionalProperties": false
}`, host.artifacts.MaxBytes())

	return Tool{
		Name: "display_artifact",
		Description: "Display an HTML artifact to the person: a chart, a table, a form, a small " +
			"interactive page. Use it when what you have to say is better looked at than listened to. " +
			"Call it again with the same artifact_id to revise what they are already looking at.",
		Parameters: json.RawMessage(parameters),
		Confirm:    string(action.ConfirmNever),
		Mutating:   false,
		Channel:    ChannelArtifact,
		run: func(_ context.Context, _ string, arguments json.RawMessage) (result, error) {
			var parsed struct {
				ArtifactID string `json:"artifact_id"`
				Title      string `json:"title"`
				HTML       string `json:"html"`
			}
			if err := json.Unmarshal(arguments, &parsed); err != nil {
				return result{}, fmt.Errorf("decode arguments: %w", err)
			}
			artifact, err := host.artifacts.Put(parsed.ArtifactID, parsed.Title, parsed.HTML)
			if err != nil {
				return result{}, err
			}
			// The model is told the version so it knows the revision landed,
			// and is told nothing else. An artifact is something a person
			// looks at; reporting its bytes back into the conversation would
			// spend context on a number nobody asked about.
			output, err := json.Marshal(map[string]any{
				"artifact_id": artifact.ID,
				"version":     artifact.Version,
				"status":      "displayed",
			})
			if err != nil {
				return result{}, err
			}
			return result{Output: string(output), Artifact: artifact}, nil
		},
	}
}

// Declarations renders the tools in the shape session.update wants.
//
// The confirmation requirement travels with the definition, so what the model
// is told about a tool and what this process will enforce are one statement.
func Declarations(tools []Tool) []map[string]any {
	declared := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		declared = append(declared, map[string]any{
			"type":         "function",
			"name":         tool.Name,
			"description":  tool.Description,
			"parameters":   json.RawMessage(tool.Parameters),
			"openrealtime": map[string]any{"confirm": tool.Confirm},
		})
	}
	return declared
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
