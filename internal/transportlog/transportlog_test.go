package transportlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/internal/broker"
)

func readRecords(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r), "every line is one JSON record")
		out = append(out, r)
	}
	return out
}

var t0 = time.Date(2026, 11, 10, 17, 4, 57, 0, time.UTC)

// TestWriter_RecordsTransitions: the transitions a failing connect produces
// land as one record each, with the state, the attempt, the error class,
// the delay, the host, and the timestamp.
func TestWriter_RecordsTransitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport.jsonl")
	w, err := Open(path, DefaultMaxBytes)
	require.NoError(t, err)
	defer func() { _ = w.Close() }()

	host := "abc123.iot.us-east-1.amazonaws.com"
	for _, tr := range []broker.Transition{
		{Kind: broker.TransitionConnecting, At: t0, Host: host, Attempt: 1},
		{Kind: broker.TransitionDegraded, At: t0.Add(time.Second), Host: host, Attempt: 1, Since: t0.Add(time.Second),
			Err: errors.New("connect: network Error : dial tcp: connection refused"), RetryIn: 2 * time.Second},
		{Kind: broker.TransitionConnecting, At: t0.Add(3 * time.Second), Host: host, Attempt: 2},
		{Kind: broker.TransitionHealthy, At: t0.Add(4 * time.Second), Host: host},
	} {
		require.NoError(t, w.Log(FromTransition(tr)))
	}

	got := readRecords(t, path)
	require.Len(t, got, 4)

	assert.Equal(t, "connecting", got[0].Event)
	assert.Equal(t, 1, got[0].Attempt)
	assert.Equal(t, host, got[0].Host)
	assert.Equal(t, t0, got[0].At)

	assert.Equal(t, "degraded", got[1].Event)
	assert.Equal(t, "connect", got[1].ErrorClass)
	assert.Equal(t, "connect: network Error : dial tcp: connection refused", got[1].Error)
	assert.Equal(t, "2s", got[1].RetryIn)
	assert.Equal(t, t0.Add(time.Second).Format(time.RFC3339), got[1].Since)

	assert.Equal(t, "connecting", got[2].Event)
	assert.Equal(t, 2, got[2].Attempt)

	assert.Equal(t, "healthy", got[3].Event)
	assert.Empty(t, got[3].Error)
	assert.Empty(t, got[3].RetryIn)
}

// TestWriter_BoundedUnderFlapping: a transport that fails every second for
// a day must not fill the disk. The current file never passes the cap, and
// one rotated file is kept, so the total is bounded by twice the cap.
func TestWriter_BoundedUnderFlapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport.jsonl")
	const capBytes = 8 << 10
	w, err := Open(path, capBytes)
	require.NoError(t, err)
	defer func() { _ = w.Close() }()

	tr := broker.Transition{Kind: broker.TransitionDegraded, Host: "relay.example.com", Attempt: 1,
		Err: errors.New("connection lost: websocket: close 1006 (abnormal closure): unexpected EOF"), RetryIn: time.Second}
	for i := 0; i < 5000; i++ {
		tr.At = t0.Add(time.Duration(i) * time.Second)
		require.NoError(t, w.Log(FromTransition(tr)))
	}

	cur, err := os.Stat(path)
	require.NoError(t, err)
	assert.LessOrEqual(t, cur.Size(), int64(capBytes), "the current file stays under the cap")
	old, err := os.Stat(RotatedPath(path))
	require.NoError(t, err, "one rotated generation is kept")
	assert.LessOrEqual(t, old.Size(), int64(capBytes))

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 2, "only the current file and one rotated file exist")

	got := readRecords(t, path)
	require.NotEmpty(t, got)
	assert.Equal(t, t0.Add(4999*time.Second), got[len(got)-1].At, "the newest record is in the current file")
}

// TestWriter_RedactsPresignedURL is the credential test: a connect error
// that quotes the SigV4-presigned URL must reach the file with the host
// only. The signature, the credential and the session token never do.
func TestWriter_RedactsPresignedURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport.jsonl")
	w, err := Open(path, DefaultMaxBytes)
	require.NoError(t, err)
	defer func() { _ = w.Close() }()

	presigned := "wss://abc123.iot.us-east-1.amazonaws.com/mqtt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE%2F20261110%2Fus-east-1%2Fiotdevicegateway%2Faws4_request&X-Amz-Date=20261110T170457Z&X-Amz-Signature=deadbeef0123&X-Amz-Security-Token=FwoGZXIvYXdzEBYaDEXAMPLE"
	tr := broker.Transition{Kind: broker.TransitionDegraded, At: t0, Host: "abc123.iot.us-east-1.amazonaws.com", Attempt: 1,
		Err: errors.New("connect: dial " + presigned + ": websocket: bad handshake (Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.secret)")}
	require.NoError(t, w.Log(FromTransition(tr)))
	require.NoError(t, w.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(raw)
	for _, secret := range []string{"X-Amz-Signature", "deadbeef0123", "AKIAEXAMPLE", "X-Amz-Security-Token", "FwoGZXIvYXdz", "eyJhbGciOiJIUzI1NiJ9", "/mqtt?"} {
		assert.NotContains(t, text, secret)
	}
	assert.Contains(t, text, `"host":"abc123.iot.us-east-1.amazonaws.com"`)
	assert.Contains(t, text, "websocket: bad handshake")
	assert.Contains(t, text, `"error_class":"connect"`)
}

// TestSummarize reads the state doctor reports back out of the file.
func TestSummarize(t *testing.T) {
	host := "relay.example.com"
	write := func(t *testing.T, trs ...broker.Transition) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "transport.jsonl")
		w, err := Open(path, DefaultMaxBytes)
		require.NoError(t, err)
		for _, tr := range trs {
			tr.Host = host
			require.NoError(t, w.Log(FromTransition(tr)))
		}
		require.NoError(t, w.Close())
		return path
	}

	t.Run("healthy", func(t *testing.T) {
		path := write(t,
			broker.Transition{Kind: broker.TransitionConnecting, At: t0, Attempt: 1},
			broker.Transition{Kind: broker.TransitionHealthy, At: t0.Add(time.Second)},
		)
		s, err := Summarize(path)
		require.NoError(t, err)
		assert.Equal(t, "healthy", s.State)
		assert.Equal(t, host, s.Host)
		assert.Equal(t, t0.Add(time.Second), s.Since)
		assert.Equal(t, t0.Add(time.Second), s.LastHealthy)
		assert.Empty(t, s.LastError)
		assert.False(t, s.Retrying)
		assert.False(t, s.OutageOpen)
	})

	t.Run("failing with an open outage", func(t *testing.T) {
		lost := errors.New("connection lost: websocket: close 1006 (abnormal closure): unexpected EOF")
		path := write(t,
			broker.Transition{Kind: broker.TransitionConnecting, At: t0, Attempt: 1},
			broker.Transition{Kind: broker.TransitionHealthy, At: t0.Add(time.Second)},
			broker.Transition{Kind: broker.TransitionDegraded, At: t0.Add(time.Minute), Attempt: 1, Err: lost, RetryIn: 2 * time.Second, Since: t0.Add(time.Minute)},
			broker.Transition{Kind: broker.TransitionConnecting, At: t0.Add(time.Minute + 2*time.Second), Attempt: 2},
			broker.Transition{Kind: broker.TransitionDegraded, At: t0.Add(time.Minute + 3*time.Second), Attempt: 2, Err: errors.New("connect: dial tcp: i/o timeout"), RetryIn: 4 * time.Second, Since: t0.Add(time.Minute)},
			broker.Transition{Kind: broker.TransitionOutage, At: t0.Add(3 * time.Minute), Attempt: 6, Since: t0.Add(time.Minute)},
			broker.Transition{Kind: broker.TransitionConnecting, At: t0.Add(3*time.Minute + time.Second), Attempt: 7},
		)
		s, err := Summarize(path)
		require.NoError(t, err)
		assert.Equal(t, "connecting", s.State)
		assert.Equal(t, t0.Add(3*time.Minute+time.Second), s.Since)
		assert.Equal(t, 7, s.Attempt)
		assert.Equal(t, t0.Add(time.Second), s.LastHealthy)
		assert.Equal(t, "connect: dial tcp: i/o timeout", s.LastError, "the message, once, as the record carries it")
		assert.Equal(t, "connect", s.LastErrorClass)
		assert.Equal(t, t0.Add(time.Minute+3*time.Second), s.LastErrorAt)
		assert.True(t, s.Retrying)
		assert.True(t, s.OutageOpen)
		assert.Equal(t, t0.Add(time.Minute), s.DownSince)
	})

	t.Run("degraded and waiting to retry", func(t *testing.T) {
		path := write(t,
			broker.Transition{Kind: broker.TransitionConnecting, At: t0, Attempt: 1},
			broker.Transition{Kind: broker.TransitionDegraded, At: t0.Add(time.Second), Attempt: 1, Err: errors.New("connect: refused"), RetryIn: 2 * time.Second, Since: t0.Add(time.Second)},
		)
		s, err := Summarize(path)
		require.NoError(t, err)
		assert.Equal(t, "degraded", s.State)
		assert.True(t, s.Retrying)
		assert.Equal(t, 2*time.Second, s.RetryIn)
		assert.True(t, s.LastHealthy.IsZero(), "never connected")
		assert.Equal(t, t0.Add(time.Second), s.DownSince)
	})

	t.Run("recovered closes the outage", func(t *testing.T) {
		path := write(t,
			broker.Transition{Kind: broker.TransitionDegraded, At: t0, Attempt: 1, Err: errors.New("connect: refused"), RetryIn: 2 * time.Second, Since: t0},
			broker.Transition{Kind: broker.TransitionOutage, At: t0.Add(2 * time.Minute), Attempt: 6, Since: t0},
			broker.Transition{Kind: broker.TransitionHealthy, At: t0.Add(3 * time.Minute)},
			broker.Transition{Kind: broker.TransitionRecovered, At: t0.Add(3 * time.Minute), Attempt: 7, Since: t0},
		)
		s, err := Summarize(path)
		require.NoError(t, err)
		assert.Equal(t, "healthy", s.State)
		assert.False(t, s.OutageOpen)
		assert.False(t, s.Retrying)
		assert.True(t, s.DownSince.IsZero())
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := Summarize(filepath.Join(t.TempDir(), "none.jsonl"))
		assert.True(t, errors.Is(err, os.ErrNotExist))
	})

	t.Run("reads the rotated file for the last healthy time", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "transport.jsonl")
		w, err := Open(path, 2<<10)
		require.NoError(t, err)
		require.NoError(t, w.Log(FromTransition(broker.Transition{Kind: broker.TransitionHealthy, At: t0, Host: host})))
		tr := broker.Transition{Kind: broker.TransitionDegraded, Host: host, Attempt: 1, Err: errors.New("connect: refused"), RetryIn: time.Second, Since: t0.Add(time.Second)}
		// A record is about 180 bytes, so a 2 KiB cap rotates once in 16
		// records: the healthy record ends up in the rotated file.
		for i := 1; i < 16; i++ {
			tr.At = t0.Add(time.Duration(i) * time.Second)
			tr.Attempt = i
			require.NoError(t, w.Log(FromTransition(tr)))
		}
		require.NoError(t, w.Close())
		_, err = os.Stat(RotatedPath(path))
		require.NoError(t, err, "the test needs a rotation to have happened")
		cur := readRecords(t, path)
		require.NotEmpty(t, cur)
		assert.NotEqual(t, "healthy", cur[0].Event, "the test needs the healthy record out of the current file")

		s, err := Summarize(path)
		require.NoError(t, err)
		assert.Equal(t, t0, s.LastHealthy, "the healthy record is in the rotated file")
		assert.Equal(t, 15, s.Attempt)
	})

	t.Run("a torn line is skipped", func(t *testing.T) {
		path := write(t, broker.Transition{Kind: broker.TransitionHealthy, At: t0})
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString(`{"at":"2026-11-10T17:05:00Z","event":"degra`)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		s, err := Summarize(path)
		require.NoError(t, err)
		assert.Equal(t, "healthy", s.State)
	})
}

func TestDescribe(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	t.Run("healthy", func(t *testing.T) {
		s := Summary{Host: "relay.example.com", State: "healthy", Since: t0, LastHealthy: t0, LastRecord: t0}
		lines := s.Describe(now)
		require.NotEmpty(t, lines)
		assert.Contains(t, lines[0], "healthy")
		assert.Contains(t, lines[0], "relay.example.com")
		assert.Contains(t, strings.Join(lines, "\n"), "10m")
	})
	t.Run("failing", func(t *testing.T) {
		s := Summary{Host: "relay.example.com", State: "degraded", Since: t0.Add(9 * time.Minute), Attempt: 8,
			LastHealthy: t0, LastError: "connection lost: websocket: close 1006 (abnormal closure): unexpected EOF", LastErrorClass: "websocket-close-1006", LastErrorAt: t0.Add(9 * time.Minute),
			Retrying: true, RetryIn: 60 * time.Second, DownSince: t0.Add(time.Minute), OutageOpen: true, LastRecord: t0.Add(9 * time.Minute)}
		text := strings.Join(s.Describe(now), "\n")
		assert.Contains(t, text, "degraded")
		assert.Contains(t, text, "attempt 8")
		assert.Contains(t, text, "retrying in 1m0s")
		assert.Contains(t, text, "last error 1m0s ago (websocket-close-1006): connection lost: websocket: close 1006")
		assert.Contains(t, text, "last healthy")
		assert.Contains(t, text, "down for 9m")
		assert.Contains(t, text, "watches were told")
	})
}

// TestDescribeWithNoRecords pins the fix for a summary that never happened.
// Open creates the file before the first record is written, so a fresh log
// summarizes to a zero Summary; Doctor then rendered " for 2562047h47m16.854775807s"
// from the zero time instead of saying there is nothing to report.
func TestDescribeWithNoRecords(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	var s Summary
	lines := s.Describe(now)
	require.NotEmpty(t, lines)
	text := strings.Join(lines, "\n")
	assert.NotContains(t, text, "2562047h", "a zero time must not be rendered as an age")
	assert.NotContains(t, text, " for ", "there is no state to have lasted")
	assert.Contains(t, text, "no records", "the reader is told the log has nothing yet")
}
