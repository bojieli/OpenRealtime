// Package gptlive presents OpenAI's GPT-Live API as a Realtime endpoint.
//
// GPT-Live shares a vendor with the Realtime API and almost nothing else. It is
// full duplex - it listens while it speaks - so it has no turn loop to drive:
// there is no input-buffer commit, no response.create that starts a spoken
// turn, and, the difference that shapes this whole package, no event marking
// the end of anything. Transcripts arrive as fragments with timestamps and stop
// arriving; the protocol says in as many words that they "do not define
// complete turns or include a transcript-done event". So this is a translator
// rather than a profile, and most of it is the boundary synthesis in turns.go.
//
// Its second difference is an opportunity rather than a cost. GPT-Live does not
// reason or call tools itself: it delegates that work and carries on talking
// while it waits. With delegation.type "client" the endpoint asks *this*
// process for help, which is precisely the seam the upstream binding exists to
// fill. So the binding's background reasoner is not bolted onto the side of a
// model that would rather answer by itself - it is the backend the vendor's own
// design expects, and the hand-off goes back through session.commentary.append,
// the channel built for exactly that.
//
// Four of the endpoint's constraints are load-bearing here:
//
//   - The model, instruction, voice, and audio format are settable only in the
//     opening session.start. So the handshake is deferred until the caller's
//     first session.update arrives, and anything sent before that is held
//     rather than dropped. Later session.updates cannot change those fields and
//     are accepted and dropped - an append is capped at 500 tokens and would
//     reject a full instruction anyway.
//   - Nothing is sent until session.started arrives, which the vendor requires
//     and which this package enforces rather than trusting call order.
//   - There are no turn boundaries, so they are synthesised. See turns.go.
//   - A completed answer is spoken by appending commentary, not by creating a
//     conversation item. The catalogue still declares the portable hand-off;
//     this translator is what makes it mean something here, which is the same
//     arrangement Gemini Live has.
package gptlive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/pcm"
	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

const (
	// DefaultURL is the Live session WebSocket. It takes no query parameters:
	// the model travels inside session.start, not in the URL, which is the
	// opposite of every Realtime endpoint in the catalogue.
	DefaultURL = "wss://api.openai.com/v1/live/sessions"
	// DefaultModel is the Live model. Like every default model in this project
	// it is a hint; gpt-live-1 serves only the v1/live/sessions endpoint.
	DefaultModel = "gpt-live-1"
	// DefaultVoice is the endpoint's own default, named here so that what this
	// package sends is visible rather than implied.
	DefaultVoice = "marin"

	// FormatPCM, FormatMuLaw and FormatALaw are the session audio formats a
	// Live WebSocket accepts. PCM is 16-bit at 24 or 16 kHz; the two G.711 laws
	// are one byte per sample at 8 kHz, which is what a telephone call is.
	FormatPCM   = "audio/pcm"
	FormatMuLaw = "audio/pcmu"
	FormatALaw  = "audio/pcma"
	// TelephonySampleRateHz is the only rate G.711 comes in.
	TelephonySampleRateHz = uint32(8_000)

	// WireSampleRateHz is what the Realtime wire carries, and what this
	// package asks the Live session for. Live accepts 16 kHz as well, so the
	// rate is configurable - but choosing the wire's rate means the audio is
	// passed through in both directions rather than resampled twice for
	// nothing.
	WireSampleRateHz = uint32(24_000)
	// AlternateSampleRateHz is the other PCM rate a Live session accepts.
	AlternateSampleRateHz = uint32(16_000)

	// appendTokenLimit is the vendor's cap on one instructions, thinking, or
	// commentary append. It is a token count and this package can only measure
	// characters, which is why commentaryChunkRunes is well under four
	// characters per token.
	appendTokenLimit = 500
	// commentaryChunkRunes bounds one commentary append. An answer longer than
	// this is split across appends rather than sent whole and rejected: the
	// vendor documents that "repeated result appends can continue the same
	// client delegation", and losing a hand-off is losing the only thing this
	// binding contributes.
	commentaryChunkRunes = 1400

	defaultDialTimeout = 30 * time.Second
	// A send here is one frame of audio or one control message. Five seconds
	// is past any stall a congested link produces and far short of a wait a
	// speaking person would sit through.
	defaultWriteTimeout = 5 * time.Second
	defaultReadLimit    = int64(8 << 20)
	// defaultCloseTimeout bounds the wait for session.closed, which is the
	// only event that confirms final usage. Waiting forever would hold a
	// session shutdown open on a vendor that has stopped answering; not
	// waiting at all would throw away the billing record on every clean exit.
	defaultCloseTimeout = 2 * time.Second
	// pendingLimit bounds audio held while the handshake completes. It is
	// generous for the same reason Gemini's is: the handshake is one round
	// trip, and the alternative to holding is losing the opening syllable.
	pendingLimit = 256
)

// Config configures one translated connection.
type Config struct {
	// URL is the Live session endpoint. Empty selects DefaultURL.
	URL string
	// APIKey is sent as Authorization: Bearer.
	APIKey string
	// Model is the Live model. Empty selects DefaultModel.
	Model string
	// Voice is the Live voice. Empty selects the endpoint's own default.
	Voice string
	// Header carries additional request headers.
	Header http.Header
	// SessionFormat is the audio format the session is opened with. Empty
	// selects PCM. The G.711 laws fix the rate at 8 kHz and are for a session
	// whose other end is a telephone: the caller's 24 kHz is resampled and
	// companded on the way in, and expanded and resampled on the way out.
	SessionFormat string
	// SessionSampleRateHz is the PCM rate the Live session is opened with.
	// Empty selects WireSampleRateHz, which needs no resampling. Ignored for
	// G.711, which has one rate.
	SessionSampleRateHz uint32
	// ForkOf names a stored session to continue from instead of starting a
	// new one. The connection goes to that session's fork endpoint, the
	// start carries only the audio format, and everything else - model,
	// instruction, voice, delegation, history - is inherited from the vendor's
	// recording. The fork gets a session id of its own.
	ForkOf string
	// Reconnect forks the session when the connection drops, which needs
	// Store and a session that has started. Nil selects on when Store is set.
	// A reconnect is the vendor's own recovery guidance: fork the stored
	// session rather than start cold, then reconcile what was pending.
	Reconnect *bool
	// MaxReconnects bounds how many forks one Client will attempt. Zero
	// selects three.
	MaxReconnects int
	// CallerSampleRateHz is the rate the caller sends and expects audio at.
	// Empty selects the Realtime wire's 24 kHz.
	CallerSampleRateHz uint32
	// FrameInterval is how often input audio must reach the endpoint for its
	// clock to advance. Zero selects the default; a negative value turns the
	// gap filler off, which only a caller that guarantees its own continuous
	// stream should do. See frames.go for why this exists at all.
	FrameInterval time.Duration
	// InputTurnGap is the silence after a user transcript fragment that ends
	// the user's turn. Zero selects the default; see turns.go for why this
	// number exists at all and what it is trading off.
	InputTurnGap time.Duration
	// OutputTurnGap is the equivalent for the assistant's own speech.
	OutputTurnGap time.Duration
	// DialTimeout bounds connection establishment.
	DialTimeout time.Duration
	// WriteTimeout bounds one send on an established connection. Zero selects
	// the shipped default; a negative value removes the bound.
	WriteTimeout time.Duration
	// CloseTimeout bounds the wait for session.closed during Close. Zero
	// selects the default; a negative value does not wait.
	CloseTimeout time.Duration
	// Store asks the endpoint to keep a resumable recording, which is what
	// makes a session forkable after a dropped connection and downloadable
	// afterwards. Off by default, as it is at the vendor; under Zero Data
	// Retention the endpoint treats it as false whatever is sent.
	Store bool
	// StallTimeout is how long a started session may send nothing before a
	// stall is reported. Zero selects the default; negative disables it.
	StallTimeout time.Duration
	// ReadLimit bounds one inbound message.
	ReadLimit int64
	// HTTPClient dials the WebSocket.
	HTTPClient *http.Client
	// Scheduler drives the turn-boundary timers. Empty selects the wall
	// clock; a test supplies a manual one so that boundaries - which are the
	// whole difficulty of this adapter - are checked deterministically rather
	// than by sleeping.
	Scheduler clock.Scheduler
}

// Client is a Live connection wearing the Realtime protocol.
type Client struct {
	config     Config
	connection *websocket.Conn

	events chan realtimeclient.Event
	closed chan struct{}
	done   chan struct{}
	// finalised is closed when session.closed arrives, which is the only
	// confirmation that usage is final.
	finalised chan struct{}
	finalOnce sync.Once

	readErr error
	errMu   sync.Mutex
	once    sync.Once

	writeMu   sync.Mutex
	startSent bool
	started   bool
	pending   [][]byte
	// handoff buffers the conversation item the binding creates, which is sent
	// as commentary when the matching response.create arrives.
	handoff strings.Builder
	// toLive and fromLive convert between the caller's rate and the session's.
	// Both are nil when the rates match, which is the default.
	toLive   *pcm.Resampler
	fromLive *pcm.Resampler
	// compand and expand are the G.711 law in force, or nil for PCM.
	compand func([]byte) []byte
	expand  func([]byte) []byte
	// forking marks that the next session.start continues a stored session.
	forking bool
	// sideband marks a client attached to a session it does not own: it
	// carries no audio and starts nothing.
	sideband   bool
	reconnects int
	dialURL    string
	dialHeader http.Header
	// silence is one pre-encoded frame used to keep the endpoint's clock
	// running through a caller's pauses.
	silence string
	// callerAppended records that the caller supplied audio since the last
	// tick, which is what makes the filler fill gaps rather than pad speech.
	callerAppended bool
	frameTimer     clock.Timer
	stallTimer     clock.Timer
	// sessionID is the vendor's identity for this session, needed to attach
	// a sideband, fork it, or fetch its recording.
	sessionID string
	// startInstruction is what session.start carried, so a later
	// session.update can be reduced to what actually changed.
	startInstruction string

	turnMu sync.Mutex
	turn   turnState
}

// Dial opens the connection and starts translating.
func Dial(ctx context.Context, config Config) (*Client, error) {
	client, err := prepare(config)
	if err != nil {
		return nil, err
	}
	target := client.dialURL
	if client.config.ForkOf != "" {
		target = forkURL(client.dialURL, client.config.ForkOf)
		client.forking = true
	}
	connection, err := client.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	client.connection = connection
	go client.read(ctx)
	return client, nil
}

// prepare validates the configuration and builds a client that has not yet
// dialled anything.
func prepare(config Config) (*Client, error) {
	if config.URL == "" {
		config.URL = DefaultURL
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return nil, errors.New("GPT-Live URL must be an absolute ws or wss URL")
	}
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("GPT-Live requires an API key")
	}
	if config.Model = strings.TrimSpace(config.Model); config.Model == "" {
		config.Model = DefaultModel
	}
	switch config.SessionFormat = strings.TrimSpace(strings.ToLower(config.SessionFormat)); config.SessionFormat {
	case "", "pcm", "pcm16", FormatPCM:
		config.SessionFormat = FormatPCM
	case "pcmu", "ulaw", "mulaw", "g711u", FormatMuLaw:
		config.SessionFormat = FormatMuLaw
		config.SessionSampleRateHz = TelephonySampleRateHz
	case "pcma", "alaw", "g711a", FormatALaw:
		config.SessionFormat = FormatALaw
		config.SessionSampleRateHz = TelephonySampleRateHz
	default:
		return nil, fmt.Errorf("GPT-Live audio format is pcm, pcmu, or pcma, not %q", config.SessionFormat)
	}
	if config.Voice = strings.TrimSpace(config.Voice); config.Voice == "" {
		config.Voice = DefaultVoice
	}
	if config.SessionSampleRateHz == 0 {
		config.SessionSampleRateHz = WireSampleRateHz
	}
	if config.SessionFormat == FormatPCM &&
		config.SessionSampleRateHz != WireSampleRateHz && config.SessionSampleRateHz != AlternateSampleRateHz {
		return nil, fmt.Errorf("GPT-Live PCM audio is %d or %d Hz, not %d",
			AlternateSampleRateHz, WireSampleRateHz, config.SessionSampleRateHz)
	}
	if config.MaxReconnects <= 0 {
		config.MaxReconnects = 3
	}
	config.ForkOf = strings.TrimSpace(config.ForkOf)
	if config.CallerSampleRateHz == 0 {
		config.CallerSampleRateHz = WireSampleRateHz
	}
	if config.FrameInterval == 0 {
		config.FrameInterval = defaultFrameInterval
	}
	if config.InputTurnGap <= 0 {
		config.InputTurnGap = defaultInputTurnGap
	}
	if config.OutputTurnGap <= 0 {
		config.OutputTurnGap = defaultOutputTurnGap
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.CloseTimeout == 0 {
		config.CloseTimeout = defaultCloseTimeout
	}
	if config.StallTimeout == 0 {
		config.StallTimeout = defaultStallTimeout
	}
	if config.ReadLimit <= 0 {
		config.ReadLimit = defaultReadLimit
	}
	if config.Scheduler == nil {
		config.Scheduler = clock.NewSystem()
	}

	client := &Client{
		config: config,
		events: make(chan realtimeclient.Event, 256),
		closed: make(chan struct{}), done: make(chan struct{}),
		finalised: make(chan struct{}),
	}
	if config.CallerSampleRateHz != config.SessionSampleRateHz {
		if client.toLive, err = pcm.NewResampler(
			config.CallerSampleRateHz, config.SessionSampleRateHz); err != nil {
			return nil, fmt.Errorf("configure GPT-Live input resampler: %w", err)
		}
		if client.fromLive, err = pcm.NewResampler(
			config.SessionSampleRateHz, config.CallerSampleRateHz); err != nil {
			return nil, fmt.Errorf("configure GPT-Live output resampler: %w", err)
		}
	}

	switch config.SessionFormat {
	case FormatMuLaw:
		client.compand, client.expand = pcm.EncodeMuLaw, pcm.DecodeMuLaw
	case FormatALaw:
		client.compand, client.expand = pcm.EncodeALaw, pcm.DecodeALaw
	}

	header := config.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	header.Set("Authorization", "Bearer "+config.APIKey)
	client.dialURL, client.dialHeader = parsed.String(), header
	return client, nil
}

// dial opens one WebSocket to the endpoint.
func (client *Client) dial(ctx context.Context, target string) (*websocket.Conn, error) {
	dialContext, cancel := context.WithTimeout(ctx, client.config.DialTimeout)
	defer cancel()
	connection, _, err := websocket.Dial(dialContext, target, &websocket.DialOptions{
		HTTPHeader: client.dialHeader.Clone(), HTTPClient: client.config.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("dial GPT-Live: %w", err)
	}
	connection.SetReadLimit(client.config.ReadLimit)
	return connection, nil
}

// forkURL is where a stored session is continued from.
func forkURL(base, sessionID string) string {
	return strings.TrimRight(base, "/") + "/" + sessionID + "/fork"
}

// reconnectable reports whether a dropped connection should be forked rather
// than reported.
func (client *Client) reconnectable() bool {
	if client.config.Reconnect != nil {
		return *client.config.Reconnect
	}
	return client.config.Store
}

// Events yields translated Realtime server events.
func (client *Client) Events() <-chan realtimeclient.Event { return client.events }

// Err reports why reading stopped, or nil for a clean close.
func (client *Client) Err() error {
	client.errMu.Lock()
	defer client.errMu.Unlock()
	return client.readErr
}

// Close ends the session and then the connection.
//
// The vendor's own guidance is that session.closed is the only event that
// establishes finalisation and carries final usage, and that dropping the
// socket first prevents its delivery. So the close command is sent, the event
// is waited for within a bound, and the socket is released either way - a
// vendor that has stopped answering must not hold a session shutdown open.
func (client *Client) Close() error {
	client.once.Do(func() {
		if !client.sideband && client.requestClose() && client.config.CloseTimeout > 0 {
			timer := time.NewTimer(client.config.CloseTimeout)
			defer timer.Stop()
			select {
			case <-client.finalised:
			case <-client.done:
			case <-timer.C:
			}
		}
		close(client.closed)
	})
	client.stopTurnTimers()
	client.stopFrameClock()
	client.stopStallWatch()
	client.writeMu.Lock()
	connection := client.connection
	client.writeMu.Unlock()
	select {
	case <-client.finalised:
		// The vendor finalised the session and, on the real endpoint, hangs
		// up straight after. A close handshake with a peer that has gone
		// fails reading the frame that never comes, and reporting that as a
		// failed close would put an error on every clean, finalised exit.
		return connection.CloseNow()
	case <-client.done:
		return connection.CloseNow()
	default:
	}
	return connection.Close(websocket.StatusNormalClosure, "client closed")
}

// requestClose sends session.close, reporting whether it went out. A session
// that never started has nothing to finalise and nothing to wait for.
func (client *Client) requestClose() bool {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if !client.started {
		return false
	}
	// The caller's context is usually already cancelled by the time a session
	// is being closed, and this is the one send whose whole purpose is to
	// outlive it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()),
		max(client.config.WriteTimeout, time.Second))
	defer cancel()
	return client.writeRaw(ctx, []byte(`{"type":"session.close"}`)) == nil
}

// SessionID reports the vendor's identity for this session once it has
// started, or empty before then.
func (client *Client) SessionID() string {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	return client.sessionID
}

// Wait blocks until reading has finished.
func (client *Client) Wait() { <-client.done }

func (client *Client) fail(err error) {
	client.errMu.Lock()
	if client.readErr == nil {
		client.readErr = err
	}
	client.errMu.Unlock()
}
