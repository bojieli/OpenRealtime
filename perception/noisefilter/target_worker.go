package noisefilter

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"
)

// Status describes audio delivery, not acoustic target-presence confidence.
// Degraded is latched until a new session; it never initiates re-enrollment.
type Status struct {
	State       string  `json:"state"`
	LatePackets uint64  `json:"late_packets"`
	MutedMS     float64 `json:"muted_ms"`
	Reason      string  `json:"reason,omitempty"`
}

func (client *Client) Status() Status {
	if client.target == nil {
		return Status{State: "healthy"}
	}
	client.target.mu.Lock()
	defer client.target.mu.Unlock()
	return client.target.status
}

const targetBacklogLimit = 500 * time.Millisecond

type targetJob struct {
	pcm       []byte
	rate      uint32
	duration  time.Duration
	submitted time.Time
	result    chan targetResult
}
type targetResult struct {
	pcm      []byte
	err      error
	finished time.Time
}

type targetWorker struct {
	client     *Client
	ctx        context.Context
	cancel     context.CancelFunc
	jobs       chan targetJob
	done       chan struct{}
	mu         sync.Mutex
	pending    time.Duration
	status     Status
	closed     bool
	lateStreak int
}

func newTargetWorker(client *Client) *targetWorker {
	ctx, cancel := context.WithCancel(context.Background())
	worker := &targetWorker{client: client, ctx: ctx, cancel: cancel, jobs: make(chan targetJob, 8), done: make(chan struct{}), status: Status{State: "healthy"}}
	go worker.run()
	return worker
}

func (worker *targetWorker) run() {
	defer close(worker.done)
	for {
		select {
		case <-worker.ctx.Done():
			return
		case job := <-worker.jobs:
			// Even when the caller has already substituted silence, finish processing
			// this exact PCM in order. The late result is never delivered on a later
			// packet. This preserves the sidecar's recurrent state and sequence.
			ctx, cancel := context.WithDeadline(worker.ctx, job.submitted.Add(targetBacklogLimit))
			pcm, err := worker.client.processPacket(ctx, job.pcm, job.rate)
			cancel()
			finished := time.Now()
			worker.mu.Lock()
			worker.pending -= job.duration
			if err != nil && !worker.closed && worker.status.State != "degraded" {
				worker.status.State = "degraded"
				worker.status.Reason = err.Error()
			}
			degraded := worker.status.State == "degraded"
			worker.mu.Unlock()
			job.result <- targetResult{pcm: pcm, err: err, finished: finished}
			if degraded {
				return
			}
		}
	}
}

func (worker *targetWorker) process(ctx context.Context, pcm []byte, rate uint32) ([]byte, error) {
	started := time.Now()
	deadline := started.Add(time.Duration(worker.client.config.TimeoutMS) * time.Millisecond)
	duration := time.Duration(int64(len(pcm)/2) * int64(time.Second) / int64(rate))
	job := targetJob{pcm: bytes.Clone(pcm), rate: rate, duration: duration, submitted: started, result: make(chan targetResult, 1)}
	worker.mu.Lock()
	if worker.closed {
		worker.mu.Unlock()
		return nil, errors.New("target filter client is closed")
	}
	if worker.status.State == "degraded" {
		worker.mu.Unlock()
		return worker.mute(len(pcm), duration, false), nil
	}
	if worker.pending+duration > targetBacklogLimit {
		worker.status.State = "degraded"
		worker.status.Reason = "target filter backlog exceeded 500ms"
		worker.mu.Unlock()
		worker.cancel()
		return worker.mute(len(pcm), duration, false), nil
	}
	select {
	case worker.jobs <- job:
		worker.pending += duration
	default:
		worker.status.State = "degraded"
		worker.status.Reason = "target filter bounded queue is full"
		worker.mu.Unlock()
		worker.cancel()
		return worker.mute(len(pcm), duration, false), nil
	}
	worker.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-job.result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if result.err != nil {
			return worker.mute(len(pcm), duration, false), nil
		}
		if result.finished.After(deadline) || time.Now().After(deadline) {
			return worker.mute(len(pcm), duration, true), nil
		}
		worker.mu.Lock()
		degraded := worker.status.State == "degraded"
		if !degraded {
			worker.status.State = "healthy"
			worker.lateStreak = 0
			worker.status.Reason = ""
		}
		worker.mu.Unlock()
		if degraded {
			return worker.mute(len(pcm), duration, false), nil
		}
		return result.pcm, nil
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return worker.mute(len(pcm), duration, true), nil
	}
}

func (worker *targetWorker) mute(size int, duration time.Duration, late bool) []byte {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	worker.status.MutedMS += float64(duration) / float64(time.Millisecond)
	if late {
		worker.status.LatePackets++
		worker.lateStreak++
		if worker.status.State != "degraded" {
			worker.status.State = "recovering"
			worker.status.Reason = "late packet replaced with silence"
			if worker.lateStreak >= 8 {
				worker.status.State = "degraded"
				worker.status.Reason = "eight consecutive target filter deadlines missed"
			}
		}
	}
	return make([]byte, size)
}

func (worker *targetWorker) close() {
	worker.mu.Lock()
	worker.closed = true
	worker.mu.Unlock()
	worker.cancel()
	<-worker.done
}
