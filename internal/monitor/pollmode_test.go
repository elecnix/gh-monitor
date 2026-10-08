package monitor

import (
	"testing"
	"time"
)

func TestPollModeNamesTheRoutedModeOrTheClientInterval(t *testing.T) {
	if got := pollMode(Event{}, 5*time.Minute); got != "polling every 300s" {
		t.Fatalf("no routed mode: got %q", got)
	}
	ev := Event{PollMode: "webhook via broker-subscriber, safety check every 30m"}
	if got := pollMode(ev, 5*time.Minute); got != ev.PollMode {
		t.Fatalf("routed mode: got %q", got)
	}
}
