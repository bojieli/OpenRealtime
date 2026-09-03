package realtimecu

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
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

//go:embed testdata/tasks.html
var taskPage []byte

// Environment owns the isolated browser used by a run.
type Environment struct {
	page    *httptest.Server
	process *exec.Cmd
	profile string
	surface *browsersurface.Surface
	stderr  *boundedBuffer
	once    sync.Once
}

// EnvironmentConfig selects the browser executable. Empty discovers Chromium
// from the usual command names.
type EnvironmentConfig struct {
	Browser string
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
		_, _ = writer.Write(taskPage)
	}))
	profile, err := os.MkdirTemp("", "openrealtime-cu-browser-")
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
	stderr := &boundedBuffer{limit: 32 << 10}
	command := exec.Command(binary,
		"--headless=new",
		"--remote-debugging-port="+port,
		"--remote-allow-origins=*",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-background-networking",
		"--disable-default-apps",
		"--no-first-run",
		"--window-size=1280,720",
		"--user-data-dir="+profile,
		"about:blank",
	)
	command.Stdout = io.Discard
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		page.Close()
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("launch Chromium: %w", err)
	}
	environment := &Environment{
		page: page, process: command, profile: profile, stderr: stderr,
	}
	devtools := "http://127.0.0.1:" + port
	if err := waitForDevTools(ctx, devtools); err != nil {
		environment.Close()
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	surface, err := browsersurface.Connect(ctx, browsersurface.Config{
		DevToolsURL: devtools, Timeout: 15 * time.Second,
	})
	if err != nil {
		environment.Close()
		return nil, err
	}
	environment.surface = surface
	return environment, nil
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

// Episode is one reset browser task. Its control-plane result never enters the
// model trajectory.
type Episode struct {
	environment *Environment
	item        Case
	mu          sync.RWMutex
	started     time.Time
}

func (environment *Environment) Episode(ctx context.Context, item Case) (*Episode, error) {
	if err := item.Task.Validate(); err != nil {
		return nil, err
	}
	pageURL := environment.page.URL + "/?task=" + url.QueryEscape(item.Task.PageMode)
	if err := environment.surface.Navigate(ctx, pageURL); err != nil {
		return nil, err
	}
	return &Episode{environment: environment, item: item}, nil
}

// Ready resets the page timer at the protocol driver's readiness boundary.
func (episode *Episode) Ready(ctx context.Context) error {
	var reset bool
	if err := episode.environment.surface.Evaluate(ctx, "window.openRealtimeTask.reset()", &reset); err != nil {
		return err
	}
	if !reset {
		return errors.New("the browser task refused to reset")
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

// CaptureScreen is the actual visual input for the session.
func (episode *Episode) CaptureScreen(ctx context.Context) ([]byte, error) {
	if episode.item.Grounding == GroundingSetOfMark {
		frame, _, err := episode.environment.surface.CaptureMarked(ctx)
		return frame, err
	}
	return episode.environment.surface.Capture(ctx)
}

// CaptureCamera renders the task's physical-camera fixture. The initial frame
// is clear; at the authored cue it becomes a smoky, red-bordered workshop.
func (episode *Episode) CaptureCamera(context.Context) ([]byte, error) {
	elapsed := time.Since(episode.Started())
	hazard := elapsed >= episode.item.Task.CueAt
	canvas := image.NewRGBA(image.Rect(0, 0, 640, 480))
	fill(canvas, color.RGBA{R: 178, G: 215, B: 235, A: 255})
	fillRect(canvas, image.Rect(0, 320, 640, 480), color.RGBA{R: 113, G: 139, B: 95, A: 255})
	fillRect(canvas, image.Rect(120, 170, 520, 360), color.RGBA{R: 194, G: 198, B: 202, A: 255})
	fillRect(canvas, image.Rect(190, 220, 270, 330), color.RGBA{R: 60, G: 66, B: 73, A: 255})
	if hazard {
		for _, cloud := range []struct{ x, y, radius int }{
			{250, 175, 75}, {330, 125, 95}, {420, 185, 85}, {355, 230, 105},
		} {
			circle(canvas, cloud.x, cloud.y, cloud.radius, color.RGBA{R: 70, G: 73, B: 78, A: 255})
		}
		border(canvas, 18, color.RGBA{R: 196, G: 32, B: 32, A: 255})
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

// PageResultCode identifies a deterministic terminal condition reported by the
// browser fixture. Reason remains human-readable evidence; scoring must use the
// structured code rather than infer semantics from that prose.
type PageResultCode string

const (
	PageResultCodeBeforeCondition PageResultCode = "before_condition"
)

// PageResult is the deterministic evaluator state owned by the fixture.
type PageResult struct {
	Complete               bool           `json:"complete"`
	Success                bool           `json:"success"`
	Code                   PageResultCode `json:"code,omitempty"`
	Reason                 string         `json:"reason"`
	CompletedAtMS          float64        `json:"completed_at_ms"`
	Actions                int            `json:"actions"`
	ActionsAfterCompletion int            `json:"actions_after_completion"`
}

func clonePageResult(result PageResult) *PageResult {
	cloned := result
	return &cloned
}

func (episode *Episode) Result(ctx context.Context) (PageResult, error) {
	var result PageResult
	err := episode.environment.surface.Evaluate(ctx, "window.openRealtimeTask.result()", &result)
	return result, err
}

type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	bytes []byte
}

func (buffer *boundedBuffer) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.bytes = append(buffer.bytes, payload...)
	if len(buffer.bytes) > buffer.limit {
		buffer.bytes = append([]byte(nil), buffer.bytes[len(buffer.bytes)-buffer.limit:]...)
	}
	return len(payload), nil
}

func (buffer *boundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.bytes)
}

func fill(canvas *image.RGBA, shade color.RGBA) { fillRect(canvas, canvas.Bounds(), shade) }

func fillRect(canvas *image.RGBA, rectangle image.Rectangle, shade color.RGBA) {
	for y := rectangle.Min.Y; y < rectangle.Max.Y; y++ {
		for x := rectangle.Min.X; x < rectangle.Max.X; x++ {
			canvas.SetRGBA(x, y, shade)
		}
	}
}

func circle(canvas *image.RGBA, centerX, centerY, radius int, shade color.RGBA) {
	for y := centerY - radius; y <= centerY+radius; y++ {
		for x := centerX - radius; x <= centerX+radius; x++ {
			dx, dy := x-centerX, y-centerY
			if dx*dx+dy*dy <= radius*radius && image.Pt(x, y).In(canvas.Bounds()) {
				canvas.SetRGBA(x, y, shade)
			}
		}
	}
}

func border(canvas *image.RGBA, width int, shade color.RGBA) {
	bounds := canvas.Bounds()
	fillRect(canvas, image.Rect(0, 0, bounds.Dx(), width), shade)
	fillRect(canvas, image.Rect(0, bounds.Dy()-width, bounds.Dx(), bounds.Dy()), shade)
	fillRect(canvas, image.Rect(0, 0, width, bounds.Dy()), shade)
	fillRect(canvas, image.Rect(bounds.Dx()-width, 0, bounds.Dx(), bounds.Dy()), shade)
}
