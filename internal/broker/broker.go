// Package broker is an optional, opt-in transport for gh-monitor's shared
// daemon: instead of relying only on a fixed polling interval, it subscribes
// to an external GitHub-webhook fan-out broker (AWS IoT Core MQTT) and
// treats each matching event as a wake signal that triggers an immediate
// fetch through the daemon's existing hub.
//
// It is never a replacement for the fetch itself. A broker event only ever
// means "something happened on this repository, go check" — this package
// does not parse the webhook body and does not derive PR or CI state from
// an event. That split is deliberate: a broker's event stream can drop
// messages across a long disconnect with no reliable replay, and a watcher
// that inferred state from the stream would let a dropped event silently
// read as "nothing happened", which is the failure mode this transport
// exists to remove, not reproduce on a new wire. State always comes from an
// authoritative fetch; the broker only decides when one runs early.
//
// The package does nothing unless GH_MONITOR_BROKER_ENDPOINT is set — see
// ConfigFromEnv. A caller that never sets it pays no cost beyond the import.
package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Config configures a broker connection.
type Config struct {
	// Endpoint is the AWS IoT Core data-plane hostname (no scheme), e.g.
	// "a1b2c3d4e5.iot.us-east-1.amazonaws.com".
	Endpoint string

	// Region is the AWS region Endpoint lives in, used for SigV4 signing.
	Region string

	// Topic is the MQTT topic filter to subscribe to, e.g.
	// "github/my-org/my-repo/+" or a wildcard spanning many repositories
	// ("github/+/+/+"). The broker's normalized event carries pr_number (and
	// the owning repository) in the payload, not the topic, so a filter
	// broader than any one watched PR is fine — events for repositories or
	// PRs nobody is watching are simply not woken (see Watcher.OnWake).
	Topic string
}

// envEndpoint, envRegion, and envTopic name the environment variables
// ConfigFromEnv reads. They are gh-monitor-specific (not shared with any
// other broker subscriber) so a user can point this daemon at a broker
// independently of anything else on the machine.
const (
	envEndpoint = "GH_MONITOR_BROKER_ENDPOINT"
	envRegion   = "GH_MONITOR_BROKER_REGION"
	envTopic    = "GH_MONITOR_BROKER_TOPIC"
	envIdleCap  = "GH_MONITOR_BROKER_IDLE_CAP"

	defaultRegion = "us-east-1"
	defaultTopic  = "github/+/+/+"
)

// ConfigFromEnv reads Config from the environment. ok is false (Config is
// the zero value) when GH_MONITOR_BROKER_ENDPOINT is unset — the transport
// is opt-in, and an unset endpoint must read as "not configured", never as
// "configured but degraded". Region defaults to us-east-1 and Topic to a
// wildcard across every repository when unset; both should normally be set
// explicitly to match the broker deployment's subscriber IAM policy.
func ConfigFromEnv() (Config, bool) {
	endpoint := strings.TrimSpace(os.Getenv(envEndpoint))
	if endpoint == "" {
		return Config{}, false
	}
	region := strings.TrimSpace(os.Getenv(envRegion))
	if region == "" {
		region = defaultRegion
	}
	topic := strings.TrimSpace(os.Getenv(envTopic))
	if topic == "" {
		topic = defaultTopic
	}
	return Config{Endpoint: endpoint, Region: region, Topic: topic}, true
}

// IdleCapFromEnv reads the daemon's broker-healthy idle-poll ceiling from
// GH_MONITOR_BROKER_IDLE_CAP (seconds). It returns def when unset, empty, or
// unparsable — a bad value must never silently disable the safety-net poll
// entirely (a zero or negative cap would), so it is clamped to at least 1s.
func IdleCapFromEnv(def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envIdleCap))
	if raw == "" {
		return def
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return def
	}
	return time.Duration(secs) * time.Second
}

// Event is the normalized broker payload's envelope — only the fields this
// transport needs to decide whether, and for which PR, to wake a fetch. It
// deliberately does not parse the nested webhook payload: this transport
// never derives state from an event, only "something happened".
type Event struct {
	Source          string `json:"source"`
	RepositoryOwner string `json:"repository_owner"`
	RepositoryName  string `json:"repository_name"`
	EventType       string `json:"event_type"`
	PRNumber        int    `json:"pr_number,omitempty"`
}

// Valid reports whether evt has the minimum shape this transport trusts:
// every event must at least name its source, its repository, and its kind.
// An event that fails this check is logged and dropped rather than treated
// as a signal for any PR — an unreadable event must never look like silence
// (waking nothing) or a guess (waking the wrong PR); it must be visibly
// ignored. PRNumber is intentionally not required: check-run and
// check-suite events are keyed by commit SHA, not PR number, and still
// carry a valid, actionable repository.
func (e Event) Valid() bool {
	return e.Source != "" && e.RepositoryOwner != "" && e.RepositoryName != "" && e.EventType != ""
}

// State is the broker connection's health, reported on every transition so
// a caller can say loudly which transport is answering rather than let a
// quiet connection read as "nothing happening".
type State int

const (
	// StateConnecting is the initial state and every state entered while
	// establishing (or re-establishing) the session.
	StateConnecting State = iota
	// StateHealthy means the session is connected and subscribed: events
	// for the configured topic will be delivered as they arrive.
	StateHealthy
	// StateDegraded means the session ended (or never connected) and a
	// reconnect attempt is pending. A caller must treat this exactly like
	// "the broker is unavailable" and fall back to its own polling cadence
	// — never keep relying on the extended, broker-healthy cadence while
	// degraded.
	StateDegraded
)

// String renders the state for logs.
func (s State) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	default:
		return "connecting"
	}
}

// connectFunc opens one broker session: it calls onConnect once the
// subscription is confirmed live, delivers every event it receives to
// onEvent, and blocks until the session ends (error, clean disconnect, or
// ctx cancellation), returning the reason (nil only on ctx cancellation).
// Production wires connectMQTT; tests inject a fake session to drive the
// reconnect state machine deterministically without a live broker — the
// same "break it and watch it degrade" test a live socket cannot give
// reliably in CI.
type connectFunc func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error

// Watcher owns one broker connection and its reconnect loop.
type Watcher struct {
	cfg     Config
	connect connectFunc

	// OnState is called on every connection-state transition, always
	// before Run acts on the new state itself, so a caller wiring cadence
	// or notifications off it never observes stale health.
	OnState func(State, error)

	// OnWake is called for every valid event that reaches this watcher.
	// owner/repo/prNumber are passed through unparsed (prNumber is 0 when
	// the event carries none); the caller decides what to refresh.
	OnWake func(owner, repo string, prNumber int)

	// initialBackoff, maxBackoff, and stableAfter tune the reconnect loop.
	// Zero values fall back to the production defaults (see Run); tests
	// override them directly (same package) to keep the reconnect state
	// machine fast and deterministic instead of waiting on real minutes.
	initialBackoff time.Duration
	maxBackoff     time.Duration
	stableAfter    time.Duration
}

// NewWatcher creates a Watcher wired to the real MQTT/SigV4 connection.
func NewWatcher(cfg Config) *Watcher {
	return &Watcher{cfg: cfg, connect: connectMQTT}
}

const (
	defaultInitialBackoff = time.Second
	defaultMaxBackoff     = 60 * time.Second
	defaultStableAfter    = 5 * time.Minute
)

// Run connects, reconnects with exponential backoff on loss, and blocks
// until ctx is cancelled, returning ctx.Err(). Every state transition is
// reported via OnState before Run takes any further action, so a caller
// relying on that signal to gate fetch cadence never observes stale health.
//
// A session that stayed connected past stableAfter resets the backoff to
// initialBackoff on its next disconnect — a broker that was working fine
// and dropped once (e.g. the SigV4-presigned URL's normal ~1h expiry)
// should reconnect promptly, the same logic the standalone broker
// subscriber tool uses. A session that never stabilizes (the broker is
// genuinely unreachable — bad endpoint, no credentials, network outage)
// backs off up to maxBackoff instead of hammering a dead endpoint.
func (w *Watcher) Run(ctx context.Context) error {
	initial := w.initialBackoff
	if initial <= 0 {
		initial = defaultInitialBackoff
	}
	maxB := w.maxBackoff
	if maxB <= 0 {
		maxB = defaultMaxBackoff
	}
	stableAfter := w.stableAfter
	if stableAfter <= 0 {
		stableAfter = defaultStableAfter
	}

	backoff := initial
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.setState(StateConnecting, nil)

		var connectedAt time.Time
		onConnect := func() {
			connectedAt = time.Now()
			w.setState(StateHealthy, nil)
		}

		err := w.connect(ctx, w.cfg, onConnect, func(evt Event) {
			if !evt.Valid() {
				log.Printf("[degraded] broker: event missing required fields (payload ignored): %+v", evt)
				return
			}
			if w.OnWake != nil {
				w.OnWake(evt.RepositoryOwner, evt.RepositoryName, evt.PRNumber)
			}
		})

		if ctx.Err() != nil {
			return ctx.Err()
		}

		w.setState(StateDegraded, err)

		if !connectedAt.IsZero() && time.Since(connectedAt) >= stableAfter {
			backoff = initial
		} else {
			backoff *= 2
			if backoff > maxB {
				backoff = maxB
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (w *Watcher) setState(s State, err error) {
	if w.OnState != nil {
		w.OnState(s, err)
	}
}

// ---------------------------------------------------------------------------
// Production connection: SigV4-presigned WSS MQTT against AWS IoT Core.
// ---------------------------------------------------------------------------

const iotService = "iotdevicegateway"

// presignWebsocketURL builds a SigV4-presigned wss:// URL for IoT Core MQTT
// over WebSocket, mirroring the technique the standalone broker-subscriber
// tool uses (same signing scheme, same "iotdevicegateway" service name AWS
// IoT Core requires for this auth path).
func presignWebsocketURL(ctx context.Context, endpoint, region string) (string, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return "", fmt.Errorf("load AWS config: %w", err)
	}

	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("retrieve AWS credentials: %w", err)
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/mqtt", endpoint), nil)
	if err != nil {
		return "", err
	}

	const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	signed, _, err := v4.NewSigner().PresignHTTP(ctx, creds, req, emptyPayloadHash, iotService, region, time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("presign: %w", err)
	}
	return strings.Replace(signed, "https://", "wss://", 1), nil
}

// connectMQTT is the production connectFunc: it presigns a WebSocket URL,
// connects, subscribes to cfg.Topic, and blocks until the session ends.
func connectMQTT(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
	brokerURL, err := presignWebsocketURL(ctx, cfg.Endpoint, cfg.Region)
	if err != nil {
		return fmt.Errorf("presign websocket URL: %w", err)
	}

	done := make(chan struct{}, 1)
	var (
		mu         sync.Mutex
		sessionErr error
	)
	setErr := func(err error) {
		mu.Lock()
		if sessionErr == nil {
			sessionErr = err
		}
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	}

	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(fmt.Sprintf("gh-monitor-%d", time.Now().UnixNano())).
		SetCleanSession(true).
		SetAutoReconnect(false).
		SetOnConnectHandler(func(cl mqtt.Client) {
			go func() {
				t := cl.Subscribe(cfg.Topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
					var evt Event
					if err := json.Unmarshal(msg.Payload(), &evt); err != nil {
						log.Printf("[degraded] broker: could not parse event on %s: %v (payload ignored)", msg.Topic(), err)
						return
					}
					onEvent(evt)
				})
				t.Wait()
				if err := t.Error(); err != nil {
					setErr(fmt.Errorf("subscribe: %w", err))
					return
				}
				onConnect()
			}()
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			setErr(fmt.Errorf("connection lost: %w", err))
		})

	client := mqtt.NewClient(opts)
	t := client.Connect()
	t.Wait()
	if err := t.Error(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	select {
	case <-ctx.Done():
	case <-done:
	}
	client.Disconnect(250)

	mu.Lock()
	err = sessionErr
	mu.Unlock()
	if err != nil {
		return err
	}
	return ctx.Err()
}
