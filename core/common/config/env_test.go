package config_test

import (
	"testing"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/stretchr/testify/require"
)

// DISABLE_NATIVE_INPUT_METER is the rollback switch for input-sized holds.
func TestDisableNativeInputMeterEnvironment(t *testing.T) {
	t.Cleanup(config.ReloadEnv) // runs after the environment is restored
	t.Setenv("DISABLE_NATIVE_INPUT_METER", "true")
	config.ReloadEnv()
	require.True(t, config.DisableNativeInputMeter)
	t.Setenv("DISABLE_NATIVE_INPUT_METER", "")
	config.ReloadEnv()
	require.False(t, config.DisableNativeInputMeter, "input meters are on unless disabled")
}
