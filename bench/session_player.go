package bench

import (
	"context"
	"fmt"
	"sync"
)

// playbackPlayer is the client side of a playback receipt.
//
// A real client plays what it receives in order, and when the server says the
// response it is playing was cancelled, it stops, throws away what is still
// queued and tells the server where it stopped. That last message is the only
// evidence of what the listener actually heard. The player keeps the spans of
// each response it has scheduled on the recorder's playout clock, which is
// already the serialized clock a speaker would follow.
type playbackPlayer struct {
	mu        sync.Mutex
	responses map[string]*playedResponse
	stopped   map[string]bool
	receipts  map[string]bool // event_ids of truncations this player sent
	sequence  int
}

type playedResponse struct {
	itemID string
	spans  [][2]float64 // [start, duration] in ms on the playout clock
}

func newPlaybackPlayer() *playbackPlayer {
	return &playbackPlayer{responses: map[string]*playedResponse{},
		stopped: map[string]bool{}, receipts: map[string]bool{}}
}

// discards reports whether audio for this response must not be played.
func (player *playbackPlayer) discards(responseID string) bool {
	player.mu.Lock()
	defer player.mu.Unlock()
	return player.stopped[responseID]
}

func (player *playbackPlayer) scheduled(responseID, itemID string, startMS, durationMS float64) {
	if responseID == "" {
		return
	}
	player.mu.Lock()
	defer player.mu.Unlock()
	response := player.responses[responseID]
	if response == nil {
		response = &playedResponse{itemID: itemID}
		player.responses[responseID] = response
	}
	if response.itemID == "" {
		response.itemID = itemID
	}
	response.spans = append(response.spans, [2]float64{startMS, durationMS})
}

// stop ends playback of a cancelled response at nowMS. It returns the item,
// how much of it had played, and whether anything was still queued; with
// nothing queued the listener heard it all and there is nothing to truncate.
func (player *playbackPlayer) stop(responseID string, nowMS float64) (string, float64, bool) {
	player.mu.Lock()
	defer player.mu.Unlock()
	response := player.responses[responseID]
	player.stopped[responseID] = true
	if response == nil {
		return "", 0, false
	}
	played, total := 0.0, 0.0
	for _, span := range response.spans {
		total += span[1]
		played += min(max(nowMS-span[0], 0), span[1])
	}
	return response.itemID, played, total-played > 0.5
}

func (player *playbackPlayer) receiptID() string {
	player.mu.Lock()
	defer player.mu.Unlock()
	player.sequence++
	id := fmt.Sprintf("bench_truncate_%d", player.sequence)
	player.receipts[id] = true
	return id
}

func (player *playbackPlayer) isReceipt(eventID string) bool {
	player.mu.Lock()
	defer player.mu.Unlock()
	return eventID != "" && player.receipts[eventID]
}

// stopPlayback applies a server cancellation to the player: cut the captured
// waveform at the stop, record the receipt, and send the truncation.
func (recorder *recorder) stopPlayback(ctx context.Context, client realtimeSession, responseID string) {
	now := recorder.at()
	itemID, played, queued := recorder.player.stop(responseID, now)
	if !queued || itemID == "" {
		return
	}
	recorder.audio.cutAt(now)
	recorder.add(Moment{Kind: MomentPlaybackStopped, AtMS: now, ResponseID: responseID,
		ItemID: itemID, AudioMS: played})
	eventID := recorder.player.receiptID()
	if err := client.Send(ctx, map[string]any{
		"type": "conversation.item.truncate", "event_id": eventID,
		"item_id": itemID, "content_index": 0, "audio_end_ms": int64(played),
	}); err != nil {
		recorder.add(Moment{Kind: MomentPlaybackTruncateRefused, ItemID: itemID, Text: err.Error()})
	}
}
