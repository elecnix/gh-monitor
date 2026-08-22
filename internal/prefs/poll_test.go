package prefs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPollIntervalResolvers_Grammar covers the duration grammar shared by the
// pollInterval and idlePollCeiling preferences (issue #90): absent, "0", or
// "false" fall back to the caller's default; a positive Go duration overrides
// it; anything unparseable (including negative durations) falls back rather
// than being guessed.
func TestPollIntervalResolvers_Grammar(t *testing.T) {
	dflt := 5 * time.Minute
	cases := []struct {
		spec string
		want time.Duration
	}{
		{"", dflt},
		{"0", dflt},
		{"false", dflt},
		{"5m", 5 * time.Minute},
		{"2h", 2 * time.Hour},
		{"90s", 90 * time.Second},
		{"garbage", dflt},
		{"-5m", dflt}, // negative is never meaningful for a cadence
	}
	for _, resolver := range []struct {
		name string
		fn   func(spec string, dflt time.Duration) time.Duration
	}{{"PollInterval", PollInterval}, {"IdlePollCeiling", IdlePollCeiling}} {
		for _, tc := range cases {
			assert.Equal(t, tc.want, resolver.fn(tc.spec, dflt),
				"%s(%q) against %s", resolver.name, tc.spec, dflt)
		}
	}
}

func TestValidDurationOverride(t *testing.T) {
	for _, ok := range []string{"", "0", "false", "5m", "2h", "6h30m"} {
		assert.True(t, ValidDurationOverride(ok), "%q should be accepted", ok)
	}
	for _, bad := range []string{"garbage", "-5m", "1", "true", "5 minutes"} {
		assert.False(t, ValidDurationOverride(bad), "%q should be rejected", bad)
	}
}

// The daemon must not pause timer polling unless the operator said so: the
// effective default of pollWhenBrokerHealthy is true (today's behaviour).
func TestDefaultPreferences_PollWhenBrokerHealthy(t *testing.T) {
	assert.True(t, DefaultPreferences().PollWhenBrokerHealthy)
}

// TestLoad_PollCadenceKeys covers the overlay rules for the three poll-cadence
// keys: absent leaves defaults; present overrides; null resets to default and
// is removed from the file.
func TestLoad_PollCadenceKeys(t *testing.T) {
	base := t.TempDir()
	writePrefsFile(t, base, `{
		"pollInterval": "7m",
		"idlePollCeiling": "6h",
		"pollWhenBrokerHealthy": false
	}`)
	p, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, "7m", p.PollInterval)
	assert.Equal(t, "6h", p.IdlePollCeiling)
	assert.False(t, p.PollWhenBrokerHealthy)

	// Null resets each key to its default and removes it from the file.
	eff, err := UpdateFile(base, []byte(`{
		"pollInterval": null,
		"idlePollCeiling": null,
		"pollWhenBrokerHealthy": null
	}`))
	require.NoError(t, err)
	assert.Equal(t, "", eff.PollInterval)
	assert.Equal(t, "", eff.IdlePollCeiling)
	assert.True(t, eff.PollWhenBrokerHealthy)

	stored := readStoredFile(t, base)
	assert.NotContains(t, stored, "pollInterval")
	assert.NotContains(t, stored, "idlePollCeiling")
	assert.NotContains(t, stored, "pollWhenBrokerHealthy")
}

// A typo in a cadence spec must be rejected at set time, never become a
// silent no-op — the same guardrail selfUpdate already has.
func TestUpdateFile_RejectsInvalidPollCadenceSpecs(t *testing.T) {
	base := t.TempDir()
	_, err := UpdateFile(base, []byte(`{"pollInterval": "soon"}`))
	require.ErrorContains(t, err, "pollInterval")

	_, err = UpdateFile(base, []byte(`{"idlePollCeiling": -5}`))
	require.ErrorContains(t, err, "idlePollCeiling")

	_, err = UpdateFile(base, []byte(`{"pollWhenBrokerHealthy": "yes"}`))
	require.ErrorContains(t, err, "pollWhenBrokerHealthy")

	_, err = UpdateFile(base, []byte(`{"pollWhenBrokerHealthy": 1}`))
	require.ErrorContains(t, err, "pollWhenBrokerHealthy")
}

func TestUpdateFile_SetsPollCadenceKeys(t *testing.T) {
	base := t.TempDir()
	eff, err := UpdateFile(base, []byte(`{
		"pollInterval": "10m",
		"idlePollCeiling": "12h",
		"pollWhenBrokerHealthy": false
	}`))
	require.NoError(t, err)
	assert.Equal(t, "10m", eff.PollInterval)
	assert.Equal(t, "12h", eff.IdlePollCeiling)
	assert.False(t, eff.PollWhenBrokerHealthy)

	stored := readStoredFile(t, base)
	assert.Equal(t, "10m", stored["pollInterval"])
	assert.Equal(t, "12h", stored["idlePollCeiling"])
	assert.Equal(t, false, stored["pollWhenBrokerHealthy"])
}

// TestResolveDaemonCadence pins the daemon's preference resolution order:
// flag/default first, then the config override on top. p comes from Load (or
// DefaultPreferences), so its effective defaults apply.
func TestResolveDaemonCadence(t *testing.T) {
	flagInterval := 5 * time.Minute

	interval, ceiling, pause := ResolveDaemonCadence(DefaultPreferences(), flagInterval)
	assert.Equal(t, flagInterval, interval, "unset pollInterval keeps the flag value")
	assert.Equal(t, DefaultIdlePollCeiling, ceiling, "unset idlePollCeiling keeps the built-in ceiling")
	assert.True(t, pause, "unset pollWhenBrokerHealthy keeps timer polling while healthy")

	interval, ceiling, pause = ResolveDaemonCadence(Preferences{
		PollInterval:          "10m",
		IdlePollCeiling:       "6h",
		PollWhenBrokerHealthy: false,
	}, flagInterval)
	assert.Equal(t, 10*time.Minute, interval)
	assert.Equal(t, 6*time.Hour, ceiling)
	assert.False(t, pause)
}
