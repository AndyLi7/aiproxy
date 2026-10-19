package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Own task queue runtime settings (spec 2026-10-15 §5.2, §6.1, §7.3.1). They
// are gateway options: stored in the gateway database, synced to every
// instance every 5 seconds and changed with PUT /api/option/:key.

// NativeQueueTimeout is how long a task may wait in the queue before it is
// failed (queue_timeout) and refunded in full (owner decision 2026-10-15:
// 5 minutes). The application documents the same value; the runtime option
// NativeQueueTimeoutSeconds can only shorten it (staging drills).
const NativeQueueTimeout = 5 * time.Minute

// Option keys and their stored defaults.
const (
	NativeQueueEnabledKey        = "NativeQueueEnabled"
	NativeQueueLimitsKey         = "NativeQueueLimits"
	NativeQueueTimeoutSecondsKey = "NativeQueueTimeoutSeconds"

	NativeQueueEnabledDefault = "false"
	NativeQueueLimitsDefault  = "{}"
)

// NativeQueueTimeoutSecondsDefault is NativeQueueTimeout in seconds.
var NativeQueueTimeoutSecondsDefault = strconv.FormatInt(int64(NativeQueueTimeout/time.Second), 10)

var (
	nativeQueueEnabled        atomic.Bool
	nativeQueueLimits         atomic.Value // map[string]int
	nativeQueueTimeoutSeconds atomic.Int64
)

func init() {
	nativeQueueLimits.Store(map[string]int{})
	nativeQueueTimeoutSeconds.Store(int64(NativeQueueTimeout / time.Second))
}

// queueScopePattern is "<provider>:<pool>", e.g. "fal:default".
var queueScopePattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}:[a-z0-9_-]{1,64}$`)

// ParseNativeQueueEnabled accepts exactly "true" or "false": a typo such as
// "\"true\"" or "1" is refused on save instead of being read as false.
func ParseNativeQueueEnabled(value string) (bool, error) {
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}

	return false, errors.New(`NativeQueueEnabled must be "true" or "false"`)
}

// ParseNativeQueueLimits accepts a JSON object of scope -> in-flight limit,
// every limit at least 1. A scope missing from it is unlimited.
func ParseNativeQueueLimits(value string) (map[string]int, error) {
	var raw map[string]json.Number

	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()

	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return nil, errors.New("NativeQueueLimits must be a JSON object of scope to limit")
	}

	if decoder.More() {
		return nil, errors.New("NativeQueueLimits must be one JSON object")
	}

	limits := make(map[string]int, len(raw))
	for scope, number := range raw {
		if !queueScopePattern.MatchString(scope) {
			return nil, fmt.Errorf("NativeQueueLimits scope %q must look like provider:pool", scope)
		}

		limit, err := strconv.ParseInt(number.String(), 10, 32)
		if err != nil || limit < 1 {
			return nil, fmt.Errorf(
				"NativeQueueLimits limit for %q must be an integer of at least 1",
				scope,
			)
		}

		limits[scope] = int(limit)
	}

	return limits, nil
}

// ParseNativeQueueTimeoutSeconds accepts 1 up to NativeQueueTimeout: the
// option may only shorten the documented queue timeout.
func ParseNativeQueueTimeoutSeconds(value string) (int64, error) {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 1 || seconds > int64(NativeQueueTimeout/time.Second) {
		return 0, fmt.Errorf(
			"NativeQueueTimeoutSeconds must be an integer from 1 to %d",
			int64(NativeQueueTimeout/time.Second),
		)
	}

	return seconds, nil
}

func GetNativeQueueEnabled() bool { return nativeQueueEnabled.Load() }

func SetNativeQueueEnabled(enabled bool) { nativeQueueEnabled.Store(enabled) }

// GetNativeQueueLimits returns a copy of the per-scope in-flight limits.
func GetNativeQueueLimits() map[string]int {
	limits, _ := nativeQueueLimits.Load().(map[string]int)
	return maps.Clone(limits)
}

func SetNativeQueueLimits(limits map[string]int) {
	nativeQueueLimits.Store(maps.Clone(limits))
}

// GetNativeQueueTimeout is the queue timeout in force: NativeQueueTimeout,
// or the shorter runtime option.
func GetNativeQueueTimeout() time.Duration {
	seconds := nativeQueueTimeoutSeconds.Load()
	if seconds < 1 || time.Duration(seconds)*time.Second > NativeQueueTimeout {
		return NativeQueueTimeout
	}

	return time.Duration(seconds) * time.Second
}

func SetNativeQueueTimeoutSeconds(seconds int64) { nativeQueueTimeoutSeconds.Store(seconds) }
