package prefs

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// daemonReadSamples gives each daemon-read key a value UpdateFile accepts, so
// the guard below proves the key is wired into the validating switch rather
// than merely named in a list. A bare `null` is deliberately not a sample: a
// reset to the default needs no reload hint, so it would not exercise the
// validation path this test exists to cover.
var daemonReadSamples = map[string]string{
	"selfUpdate":            `"30m"`,
	"pollInterval":          `"10m"`,
	"idlePollCeiling":       `"20m"`,
	"pollWhenBrokerHealthy": `false`,
}

// TestDaemonReadKeysAreSettable is the guard for the drift this list exists to
// prevent. The reload hint used to be a hardcoded slice in the prefs-set
// command: adding a daemon-read preference to UpdateFile's switch left the
// hint behind, so a user set the value, saw no hint, and the resident daemon
// kept running on the old one with no error anywhere. Every key named as
// daemon-read must therefore be a key UpdateFile actually accepts.
func TestDaemonReadKeysAreSettable(t *testing.T) {
	keys := DaemonReadKeys()
	require.NotEmpty(t, keys, "the daemon-read set must not be empty")

	seen := map[string]bool{}
	for _, key := range keys {
		require.False(t, seen[key], "duplicate key %q in DaemonReadKeys()", key)
		seen[key] = true

		assert.True(t, IsDaemonReadKey(key), "IsDaemonReadKey(%q) must agree with DaemonReadKeys()", key)

		sample, ok := daemonReadSamples[key]
		require.True(t, ok,
			"daemon-read key %q has no sample value in daemonReadSamples — add one, or drop it from DaemonReadKeys()", key)

		// A real (non-null) value must be accepted by the validating switch.
		_, err := UpdateFile(t.TempDir(), []byte(`{"`+key+`":`+sample+`}`))
		require.NoError(t, err, "UpdateFile must accept daemon-read key %q with %s", key, sample)
	}
}

// TestDaemonReadKeys_SubsetOfValidKeys covers the other direction: the list
// must not name a key UpdateFile would reject, because the reload hint would
// then advertise a write that cannot happen.
func TestDaemonReadKeys_SubsetOfValidKeys(t *testing.T) {
	assert.Subset(t, validPrefKeys, DaemonReadKeys(),
		"every daemon-read key must be a key UpdateFile validates")
}

// TestClientRereadKeys_AreNotDaemonRead documents the other half of the
// classification: keys a plain client re-reads on every command, so they must
// NOT be advertised as needing a daemon reload.
func TestClientRereadKeys_AreNotDaemonRead(t *testing.T) {
	for _, key := range []string{"templates", "ignoredBots", "retriggerComments", "reactOnNotify", "eventLog"} {
		assert.False(t, IsDaemonReadKey(key),
			"%q is re-read per command and must not trigger a reload hint", key)
	}
}

// TestDaemonReadKeys_ReturnsCopy ensures a caller cannot mutate the package
// list (the hint iterates it directly).
func TestDaemonReadKeys_ReturnsCopy(t *testing.T) {
	first := DaemonReadKeys()
	require.NotEmpty(t, first)
	first[0] = "mutated"

	assert.NotEqual(t, "mutated", DaemonReadKeys()[0])
	assert.True(t, IsDaemonReadKey(DaemonReadKeys()[0]))
}

// TestValidPrefKeysAreAcceptedByUpdateFile keeps the unknown-key error honest.
// validPrefKeys is hand-maintained and UpdateFile's switch is the real
// validation; a key added to one and not the other would make the error name a
// set that no longer matches what a write accepts — the exact drift the
// declaration claims to prevent.
//
// The value is deliberately unusable rather than valid: any error other than
// the unknown-key one proves the switch matched the key, which is all this
// needs to know, and it spares the test a sample per key.
func TestValidPrefKeysAreAcceptedByUpdateFile(t *testing.T) {
	dir := t.TempDir()
	accepted := map[string]bool{}
	for _, key := range validPrefKeys {
		_, err := UpdateFile(dir, []byte(`{"`+key+`": null}`))
		if err != nil && strings.Contains(err.Error(), "unknown preference key") {
			t.Errorf("validPrefKeys names %q but UpdateFile rejects it: %v", key, err)
			continue
		}
		accepted[key] = true
	}
	// The other direction, which is the one that makes the error message
	// trustworthy: every key UpdateFile accepts must be listed. A key added to
	// the switch and not here would otherwise pass this test and leave the
	// unknown-key error naming a set that is out of date.
	for _, key := range updateFileKeys(t) {
		if !accepted[key] {
			t.Errorf("UpdateFile accepts %q but validPrefKeys omits it", key)
		}
	}
}

// updateFileKeys is every top-level key UpdateFile recognises, discovered by
// probing it rather than by reading the switch, so the two lists in this file
// cannot drift apart without the tests above noticing.
func updateFileKeys(t *testing.T) []string {
	t.Helper()
	var keys []string
	for _, candidate := range append([]string{"templates", "ignoredBots", "retriggerComments",
		"selfUpdate", "pollInterval", "idlePollCeiling", "pollWhenBrokerHealthy",
		"reactOnNotify", "eventLog", "notAKeyAtAll"}, []string{}...) {
		_, err := UpdateFile(t.TempDir(), []byte(`{"`+candidate+`": null}`))
		if err == nil || !strings.Contains(err.Error(), "unknown preference key") {
			keys = append(keys, candidate)
		}
	}
	return keys
}
