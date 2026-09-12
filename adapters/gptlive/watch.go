package gptlive

import (
	"context"
	"time"
)

// The stall watchdog turns the endpoint's own cadence into a named failure.
//
// Two of this endpoint's failure modes were silent: a session with no input
// frames does nothing and says nothing, and a session whose output is only
// carrier looks alive to a reader counting frames. What the endpoint does send
// unprompted, on every live run so far, is session.usage.updated roughly every
// ten seconds. A connected session that stops receiving it - or anything else -
// for well past that interval has stalled, and stalled is a word an operator
// can act on where silence is not.
const defaultStallTimeout = 45 * time.Second

// armStallWatch restarts the watchdog. Every server event calls it, so the
// timer only fires when the endpoint has gone quiet in every respect.
func (client *Client) armStallWatch(ctx context.Context) {
	if client.config.StallTimeout <= 0 {
		return
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if !client.started {
		return
	}
	if client.stallTimer != nil {
		client.stallTimer.Stop()
	}
	client.stallTimer = client.config.Scheduler.AfterFunc(client.config.StallTimeout, func() {
		select {
		case <-client.closed:
			return
		case <-client.finalised:
			return
		default:
		}
		// Not fatal on its own: the read side reports a connection that has
		// actually gone, and a stall is reported so that it is not mistaken
		// for a model that chose to stay quiet.
		_ = client.emit(ctx, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"code": "upstream_stalled",
				"message": "the Live session sent nothing for " + client.config.StallTimeout.String() +
					"; its usage heartbeat normally arrives every ten seconds",
			},
		})
	})
}

// stopStallWatch halts the watchdog once the session has been finalised.
func (client *Client) stopStallWatch() {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.stallTimer != nil {
		client.stallTimer.Stop()
		client.stallTimer = nil
	}
}
