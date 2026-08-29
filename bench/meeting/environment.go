package meeting

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	browsersurface "github.com/bojieli/OpenRealtime/computeruse/browser"
)

//go:embed testdata/meeting.html
var meetingPage []byte

type EnvironmentConfig struct{ Browser string }

// Environment owns one isolated Chromium process for a suite run.
type Environment struct {
	page    *httptest.Server
	process *exec.Cmd
	profile string
	surface *browsersurface.Surface
	once    sync.Once
}

func NewEnvironment(ctx context.Context, config EnvironmentConfig) (*Environment, error) {
	binary := strings.TrimSpace(config.Browser)
	if binary == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if found, err := exec.LookPath(candidate); err == nil {
				binary = found
				break
			}
		}
	}
	if binary == "" {
		return nil, errors.New("Chromium is required; pass -browser or install chromium")
	}
	page := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write(meetingPage)
	}))
	profile, err := os.MkdirTemp("", "openrealtime-meeting-browser-")
	if err != nil {
		page.Close()
		return nil, err
	}
	port, err := reservePort()
	if err != nil {
		page.Close()
		_ = os.RemoveAll(profile)
		return nil, err
	}
	command := exec.Command(binary,
		"--headless=new", "--remote-debugging-port="+port, "--remote-allow-origins=*",
		"--no-sandbox", "--disable-gpu", "--disable-background-networking",
		"--disable-default-apps", "--no-first-run", "--window-size=1280,720",
		"--user-data-dir="+profile, "about:blank",
	)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		page.Close()
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("launch Chromium: %w", err)
	}
	environment := &Environment{page: page, process: command, profile: profile}
	devtools := "http://127.0.0.1:" + port
	if err := waitForDevTools(ctx, devtools); err != nil {
		environment.Close()
		return nil, err
	}
	surface, err := browsersurface.Connect(ctx, browsersurface.Config{DevToolsURL: devtools, Timeout: 15 * time.Second})
	if err != nil {
		environment.Close()
		return nil, err
	}
	environment.surface = surface
	return environment, nil
}

func (environment *Environment) Close() error {
	var result error
	environment.once.Do(func() {
		if environment.surface != nil {
			result = errors.Join(result, environment.surface.Close())
		}
		if environment.process != nil && environment.process.Process != nil {
			_ = environment.process.Process.Kill()
			_, _ = environment.process.Process.Wait()
		}
		if environment.page != nil {
			environment.page.Close()
		}
		if environment.profile != "" {
			result = errors.Join(result, os.RemoveAll(filepath.Clean(environment.profile)))
		}
	})
	return result
}

type Episode struct {
	environment *Environment
	task        Task
	mu          sync.RWMutex
	started     time.Time
}

func (environment *Environment) Episode(ctx context.Context, task Task) (*Episode, error) {
	if err := task.Validate(); err != nil {
		return nil, err
	}
	pageURL := environment.page.URL + "/?task=" + url.QueryEscape(task.PageMode)
	if err := environment.surface.Navigate(ctx, pageURL); err != nil {
		return nil, err
	}
	return &Episode{environment: environment, task: task}, nil
}

func (episode *Episode) Ready(ctx context.Context) error {
	var reset bool
	if err := episode.environment.surface.Evaluate(ctx, "window.openRealtimeMeeting.reset()", &reset); err != nil {
		return err
	}
	if !reset {
		return errors.New("the meeting fixture refused to reset")
	}
	episode.mu.Lock()
	episode.started = time.Now()
	episode.mu.Unlock()
	return nil
}

func (episode *Episode) Started() time.Time {
	episode.mu.RLock()
	defer episode.mu.RUnlock()
	return episode.started
}

func (episode *Episode) Surface() *browsersurface.Surface { return episode.environment.surface }

func (episode *Episode) CaptureScreen(ctx context.Context) ([]byte, error) {
	frame, _, err := episode.environment.surface.CaptureMarked(ctx)
	return frame, err
}

func (episode *Episode) Result(ctx context.Context) (PageResult, error) {
	var result PageResult
	err := episode.environment.surface.Evaluate(ctx, "window.openRealtimeMeeting.result()", &result)
	return result, err
}

func reservePort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	return port, err
}

func waitForDevTools(ctx context.Context, endpoint string) error {
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("Chromium did not open its DevTools endpoint within 30s")
}
