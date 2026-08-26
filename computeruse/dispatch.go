package computeruse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Surface performs actions on a real screen.
//
// It is the only thing in this package that touches the world, and it is
// deliberately tiny: a browser context, a virtual display, or a test double
// implements nine methods and nothing else. Everything about authority,
// confirmation, targeting, and auditing happens above it.
type Surface interface {
	Name() string
	Click(ctx context.Context, x, y int, button string) error
	DoubleClick(ctx context.Context, x, y int) error
	Move(ctx context.Context, x, y int) error
	Drag(ctx context.Context, fromX, fromY, toX, toY int) error
	Type(ctx context.Context, text string) error
	Key(ctx context.Context, keys []string) error
	Scroll(ctx context.Context, x, y, deltaX, deltaY int) error
	Screenshot(ctx context.Context) error
}

// ElementSurface is the optional browser grounding extension.
//
// Pixel actions remain the portable baseline for a desktop, VM, or Android
// display. A browser can additionally expose a set-of-mark frame whose labels
// name interactive elements. Keeping that operation in a separate interface
// means a general screen surface does not pretend it can resolve DOM elements,
// and the dispatcher can refuse the wrong grounding mode explicitly.
type ElementSurface interface {
	ClickElement(ctx context.Context, elementID string) error
}

// Record is one performed action, for the audit trail.
type Record struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Target    string `json:"target"`
	Source    string `json:"source"`
	Arguments string `json:"arguments"`
	Error     string `json:"error,omitempty"`
	AtNS      uint64 `json:"at_ns"`
}

// DispatcherConfig configures action dispatch.
type DispatcherConfig struct {
	Target  Target
	Surface Surface
	// MaxWait bounds computer.wait, so a model cannot stall a session by
	// asking for an hour.
	MaxWait time.Duration
	// Audit receives every performed action.
	Audit func(Record)
	// Now supplies audit timestamps.
	Now func() uint64
}

// Dispatcher performs computer-use actions against one target.
type Dispatcher struct {
	config DispatcherConfig

	mu      sync.Mutex
	records []Record
}

// NewDispatcher validates the configuration.
func NewDispatcher(config DispatcherConfig) (*Dispatcher, error) {
	if err := config.Target.Validate(); err != nil {
		return nil, err
	}
	if config.Surface == nil {
		return nil, errors.New("computer-use dispatch requires a surface")
	}
	if config.MaxWait <= 0 {
		config.MaxWait = 10 * time.Second
	}
	if config.Now == nil {
		config.Now = func() uint64 { return uint64(time.Now().UnixNano()) }
	}
	return &Dispatcher{config: config}, nil
}

// Name identifies the dispatcher in reports.
func (dispatcher *Dispatcher) Name() string {
	return "computer-use:" + dispatcher.config.Target.Name
}

type arguments struct {
	Source    string   `json:"source"`
	ElementID string   `json:"element_id"`
	X         int      `json:"x"`
	Y         int      `json:"y"`
	Button    string   `json:"button"`
	FromX     int      `json:"from_x"`
	FromY     int      `json:"from_y"`
	ToX       int      `json:"to_x"`
	ToY       int      `json:"to_y"`
	Text      string   `json:"text"`
	Keys      []string `json:"keys"`
	DeltaX    int      `json:"delta_x"`
	DeltaY    int      `json:"delta_y"`
	Duration  int      `json:"duration_ms"`
}

// Dispatch performs one action.
//
// Every rejection here is about grounding rather than about content: does this
// target own that source, and does that coordinate exist in the space the
// model was shown. An action that lands outside the screen is not a
// near-miss - it is an action on something nobody looked at.
func (dispatcher *Dispatcher) Dispatch(ctx context.Context, call trajectory.ToolCall) (trajectory.ToolResult, error) {
	result := trajectory.ToolResult{CallID: call.CallID, Name: call.Name}
	var parsed arguments
	if err := json.Unmarshal(call.Arguments, &parsed); err != nil {
		return dispatcher.fail(result, parsed, fmt.Errorf("decode arguments: %w", err))
	}
	if call.Name != Wait {
		if strings.TrimSpace(parsed.Source) == "" {
			return dispatcher.fail(result, parsed, errors.New("an action must name a declared video source"))
		}
		if !dispatcher.config.Target.Owns(parsed.Source) {
			return dispatcher.fail(result, parsed, fmt.Errorf(
				"target %q does not own source %q", dispatcher.config.Target.Name, parsed.Source))
		}
	}

	var err error
	switch call.Name {
	case Click:
		if err = dispatcher.inside(parsed.X, parsed.Y); err == nil {
			err = dispatcher.config.Surface.Click(ctx, parsed.X, parsed.Y, button(parsed.Button))
		}
	case ClickElement:
		grounded, ok := dispatcher.config.Surface.(ElementSurface)
		if !ok {
			err = errors.New("this target does not support set-of-mark element grounding")
		} else if strings.TrimSpace(parsed.ElementID) == "" {
			err = errors.New("an element click requires a visible mark label")
		} else {
			err = grounded.ClickElement(ctx, parsed.ElementID)
		}
	case DoubleClick:
		if err = dispatcher.inside(parsed.X, parsed.Y); err == nil {
			err = dispatcher.config.Surface.DoubleClick(ctx, parsed.X, parsed.Y)
		}
	case Move:
		if err = dispatcher.inside(parsed.X, parsed.Y); err == nil {
			err = dispatcher.config.Surface.Move(ctx, parsed.X, parsed.Y)
		}
	case Drag:
		if err = dispatcher.inside(parsed.FromX, parsed.FromY); err == nil {
			if err = dispatcher.inside(parsed.ToX, parsed.ToY); err == nil {
				err = dispatcher.config.Surface.Drag(ctx, parsed.FromX, parsed.FromY, parsed.ToX, parsed.ToY)
			}
		}
	case Type:
		if parsed.Text == "" {
			err = errors.New("typing requires text")
		} else {
			err = dispatcher.config.Surface.Type(ctx, parsed.Text)
		}
	case Key:
		if len(parsed.Keys) == 0 {
			err = errors.New("a key action requires at least one key")
		} else {
			err = dispatcher.config.Surface.Key(ctx, parsed.Keys)
		}
	case Scroll:
		if err = dispatcher.inside(parsed.X, parsed.Y); err == nil {
			err = dispatcher.config.Surface.Scroll(ctx, parsed.X, parsed.Y, parsed.DeltaX, parsed.DeltaY)
		}
	case Screenshot:
		err = dispatcher.config.Surface.Screenshot(ctx)
	case Wait:
		err = dispatcher.wait(ctx, parsed.Duration)
	default:
		err = fmt.Errorf("%q is not in the computer-use namespace", call.Name)
	}
	if err != nil {
		return dispatcher.fail(result, parsed, err)
	}
	dispatcher.record(Record{
		CallID: call.CallID, Name: call.Name, Target: dispatcher.config.Target.Name,
		Source: parsed.Source, Arguments: string(call.Arguments), AtNS: dispatcher.config.Now(),
	})
	// The output is text and stays text. The visual consequence returns
	// through the video stream, which is what keeps the addition small.
	result.Output = json.RawMessage(fmt.Sprintf("%q", outcomeFor(call.Name)))
	return result, nil
}

func (dispatcher *Dispatcher) wait(ctx context.Context, milliseconds int) error {
	if milliseconds < 0 {
		return errors.New("a wait cannot be negative")
	}
	duration := time.Duration(milliseconds) * time.Millisecond
	if duration > dispatcher.config.MaxWait {
		duration = dispatcher.config.MaxWait
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func (dispatcher *Dispatcher) inside(x, y int) error {
	if x < 0 || y < 0 || x >= dispatcher.config.Target.Width || y >= dispatcher.config.Target.Height {
		return fmt.Errorf("(%d, %d) is outside the %dx%d space of target %q",
			x, y, dispatcher.config.Target.Width, dispatcher.config.Target.Height, dispatcher.config.Target.Name)
	}
	return nil
}

func (dispatcher *Dispatcher) fail(result trajectory.ToolResult, parsed arguments, err error) (trajectory.ToolResult, error) {
	dispatcher.record(Record{
		CallID: result.CallID, Name: result.Name, Target: dispatcher.config.Target.Name,
		Source: parsed.Source, Error: err.Error(), AtNS: dispatcher.config.Now(),
	})
	// A failed action returns a result rather than an error: the model must
	// see that it did not work, and a tool error is authoritative evidence.
	result.Error = err.Error()
	return result, nil
}

func (dispatcher *Dispatcher) record(record Record) {
	dispatcher.mu.Lock()
	dispatcher.records = append(dispatcher.records, record)
	dispatcher.mu.Unlock()
	if dispatcher.config.Audit != nil {
		dispatcher.config.Audit(record)
	}
}

// Records returns the audit trail.
func (dispatcher *Dispatcher) Records() []Record {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	return append([]Record(nil), dispatcher.records...)
}

func button(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "right":
		return "right"
	case "middle":
		return "middle"
	default:
		return "left"
	}
}

func outcomeFor(name string) string {
	switch name {
	case Click, DoubleClick:
		return "clicked"
	case Move:
		return "moved"
	case Drag:
		return "dragged"
	case Type:
		return "typed"
	case Key:
		return "pressed"
	case Scroll:
		return "scrolled"
	case Screenshot:
		return "captured"
	case Wait:
		return "waited"
	default:
		return "done"
	}
}

var _ action.Dispatcher = (*Dispatcher)(nil)
