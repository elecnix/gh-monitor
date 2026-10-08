package prefs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// coveredSafetyInterval differs from the cadence keys: "0" turns the check
// off instead of keeping the default.
func TestCoveredSafetyInterval_Grammar(t *testing.T) {
	cases := []struct {
		spec string
		want time.Duration
	}{
		{"", DefaultCoveredSafetyInterval},
		{"30m", 30 * time.Minute},
		{"90s", 90 * time.Second},
		{"0", 0},
		{"false", 0},
		{"garbage", DefaultCoveredSafetyInterval},
		{"-5m", DefaultCoveredSafetyInterval},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, CoveredSafetyInterval(tc.spec), "CoveredSafetyInterval(%q)", tc.spec)
	}
	assert.Equal(t, 30*time.Minute, DefaultCoveredSafetyInterval)
}

func TestUpdateFileSetsCoveredSafetyInterval(t *testing.T) {
	base := t.TempDir()
	eff, err := UpdateFile(base, []byte(`{"coveredSafetyInterval":"10m"}`))
	require.NoError(t, err)
	assert.Equal(t, "10m", eff.CoveredSafetyInterval)

	eff, err = UpdateFile(base, []byte(`{"coveredSafetyInterval":"0"}`))
	require.NoError(t, err)
	assert.Equal(t, "0", eff.CoveredSafetyInterval)
	assert.Equal(t, time.Duration(0), CoveredSafetyInterval(eff.CoveredSafetyInterval))

	_, err = UpdateFile(base, []byte(`{"coveredSafetyInterval":"soon"}`))
	assert.Error(t, err)

	eff, err = UpdateFile(base, []byte(`{"coveredSafetyInterval":null}`))
	require.NoError(t, err)
	assert.Equal(t, "", eff.CoveredSafetyInterval)
}

func TestFirstPollTemplateStatesTheMode(t *testing.T) {
	assert.Contains(t, DefaultPreferences().Templates["first-poll"], "{pollMode}")
	assert.Contains(t, RecognizedTokens(), "pollMode")
}
