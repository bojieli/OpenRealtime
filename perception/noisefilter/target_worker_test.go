package noisefilter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func targetReply(w http.ResponseWriter, r *http.Request, pcm []byte) {
	w.Header().Set("X-Sequence", r.Header.Get("X-Sequence"))
	w.Header().Set("X-Filter-Model", "real-tse")
	w.Header().Set("X-Audio-Delay-MS", "65")
	w.Header().Set("X-Target-Voice-State", "extracting")
	w.Write(pcm)
}

func TestLateTargetPacketBecomesSilenceThenRecoversWithoutReordering(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		pcm, _ := io.ReadAll(r.Body)
		n := calls.Add(1)
		if n == 1 {
			<-release
		}
		if r.Header.Get("X-Sequence") != string(rune('0'+n-1)) {
			t.Error("sequence changed after deadline")
		}
		for i := range pcm {
			pcm[i] = byte(n)
		}
		targetReply(w, r, pcm)
	}))
	defer server.Close()
	client, _ := New(Config{URL: server.URL, Model: "real-tse", TimeoutMS: 20})
	defer client.Close()
	input := bytes.Repeat([]byte{90}, 3200)
	result, err := client.Process(context.Background(), input, 16000)
	if err != nil || !bytes.Equal(result, make([]byte, len(input))) {
		t.Fatal("late packet was not muted")
	}
	if client.Status().State != "recovering" || client.Status().LatePackets != 1 {
		t.Fatal(client.Status())
	}
	clear(input) // Worker must own the input while it finishes asynchronously.
	close(release)
	result, err = client.Process(context.Background(), bytes.Repeat([]byte{80}, 3200), 16000)
	if err != nil || !bytes.Equal(result, bytes.Repeat([]byte{2}, 3200)) {
		t.Fatal("late output replayed or recovery failed", err)
	}
	if client.Status().State != "healthy" || client.Status().MutedMS != 100 {
		t.Fatal(client.Status())
	}
}

func TestTargetBacklogIsBoundedAndDegradationNeverReenrolls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(Config{URL: server.URL, Model: "real-tse", TimeoutMS: 1})
	defer client.Close()
	for i := 0; i < 10; i++ {
		result, err := client.Process(context.Background(), bytes.Repeat([]byte{99}, 3200), 16000)
		if err != nil || !bytes.Equal(result, make([]byte, 3200)) {
			t.Fatal("overload leaked audio or failed session", err)
		}
	}
	status := client.Status()
	if status.State != "degraded" || !strings.Contains(status.Reason, "backlog") {
		t.Fatal(status)
	}
	if calls.Load() > 1 {
		t.Fatal("degraded state restarted or issued concurrent requests")
	}
}

func TestClosingTargetClientCancelsOutstandingRequest(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(Config{URL: server.URL, Model: "real-tse", TimeoutMS: 10})
	client.Process(context.Background(), make([]byte, 3200), 16000)
	<-entered
	done := make(chan struct{})
	go func() { client.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close left worker running")
	}
}

func TestRepeatedTargetDeadlineMissesEnterExplicitDegradedState(t *testing.T) {
	finished := make(chan struct{}, 1)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		pcm, _ := io.ReadAll(r.Body)
		calls.Add(1)
		time.Sleep(15 * time.Millisecond)
		targetReply(w, r, pcm)
		finished <- struct{}{}
	}))
	defer server.Close()
	client, _ := New(Config{URL: server.URL, Model: "real-tse", TimeoutMS: 5})
	defer client.Close()
	for i := 0; i < 8; i++ {
		got, err := client.Process(context.Background(), bytes.Repeat([]byte{7}, 3200), 16000)
		if err != nil || !bytes.Equal(got, make([]byte, 3200)) {
			t.Fatal("deadline failed to mute", err)
		}
		<-finished
	}
	status := client.Status()
	if status.State != "degraded" || status.LatePackets != 8 || !strings.Contains(status.Reason, "consecutive") {
		t.Fatal(status)
	}
	client.Process(context.Background(), make([]byte, 3200), 16000)
	if calls.Load() != 8 {
		t.Fatal("degraded stream resumed or reenrolled")
	}
}
