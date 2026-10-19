package controller

import (
	"encoding/json"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

// Why a native submission got 503 model_route_unavailable. Reasons go to the
// server log only: customers keep the one public code, which is safe to retry
// with the same X-Request-Id because routing runs before any reservation.
// middleware.NativeRouteAmbiguousReason is the sixth.
const (
	nativeRouteBindingInvalid          = "binding_invalid"
	nativeRouteBoundChannelUnavailable = "bound_channel_unavailable"
	nativeRouteChannelIdentityMismatch = "channel_identity_mismatch"
	nativeRouteKeyFingerprintMismatch  = "key_fingerprint_mismatch"
	nativeRouteQuoteInvalid            = "quote_invalid"
)

// NativeRouteFollowsBindingFeature tells the application that native task
// routing admits only the channel a model config is bound to (rule R1, see
// nativeBindingFilter).
const NativeRouteFollowsBindingFeature = "native_route_follows_binding_v1"

// nativeRouteFeatures advertises NativeRouteFollowsBindingFeature only while
// NATIVE_ROUTE_FOLLOWS_BINDING is on and the native engine is configured, so
// switching the binding filter off withdraws the promise with it.
func nativeRouteFeatures(engineConfigured bool) []string {
	if !engineConfigured || !config.NativeRouteFollowsBinding {
		return nil
	}

	return []string{NativeRouteFollowsBindingFeature}
}

// nativeRouteError is a ResolvePlan failure with its reason. It unwraps
// to nativetask.ErrUnavailable, so the public answer is unchanged.
type nativeRouteError struct{ reason string }

func (r *nativeRouteError) Error() string { return "native route rejected: " + r.reason }
func (r *nativeRouteError) Unwrap() error { return nativetask.ErrUnavailable }

// parseNativeRouteBinding reads the model config's native binding. It is
// invalid when missing, unparsable, of another version, or naming no channel
// (the unbound placeholder a publisher may write before its channel exists).
func parseNativeRouteBinding(mc model.ModelConfig) (nativeRouteBinding, bool) {
	raw := mc.Config[NativeResultConfigKey]
	if raw == nil {
		return nativeRouteBinding{}, false
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nativeRouteBinding{}, false
	}

	var binding nativeRouteBinding
	if json.Unmarshal(encoded, &binding) != nil || binding.Version != 1 || binding.ChannelID <= 0 {
		return nativeRouteBinding{}, false
	}

	return binding, true
}

// nativeBindingFilter restricts native task routing to the channel the model
// config is bound to (rule R1). Selection used to pick among every enabled
// channel serving the model and only then compare the pick with the binding:
// while a release had two fal channels enabled, about half of all native
// submissions failed. The filter alone leaves the bound channel as the only
// candidate, so its priority and backup-only flag no longer matter. It applies
// to every path through getInitialChannel, including the Aiproxy-Channel
// header, which can name a disabled channel: the status check is for that path.
// A missing or invalid binding admits no channel; it never means "no filter".
// NATIVE_ROUTE_FOLLOWS_BINDING=false returns no filter (the old selection).
func nativeBindingFilter(c *gin.Context, m mode.Mode) func(*model.Channel) bool {
	if m != mode.NativeTasks || !config.NativeRouteFollowsBinding {
		return nil
	}

	value, _ := c.Get(middleware.ModelConfig)
	mc, _ := value.(model.ModelConfig)

	binding, ok := parseNativeRouteBinding(mc)
	if !ok {
		return func(*model.Channel) bool { return false }
	}

	return func(ch *model.Channel) bool {
		return ch != nil && ch.ID == binding.ChannelID && ch.Status == model.ChannelStatusEnabled
	}
}

// rejectNativeRoute logs why a native submission has no route and returns the
// rejection. Never logs a key; fingerprints are cut to 8 characters.
func rejectNativeRoute(
	c *gin.Context,
	reason string,
	binding nativeRouteBinding,
	channel *model.Channel,
	cause error,
) (nativetask.Plan, nativetask.Provider, error) {
	selected := 0
	if channel != nil {
		selected = channel.ID
	}

	line := fmt.Sprintf(
		"native route rejected: reason=%s request_id=%s route=%s binding_channel=%d selected_channel=%d cache_version=%d follows_binding=%t",
		reason,
		middleware.GetRequestID(c),
		middleware.GetRoutingModel(c),
		binding.ChannelID,
		selected,
		middleware.ModelCacheVersion(c),
		config.NativeRouteFollowsBinding,
	)
	if reason == nativeRouteKeyFingerprintMismatch && channel != nil {
		channelFingerprint := ""
		if channel.Key != "" {
			channelFingerprint = model.ImageChannelKeyFingerprint(channel.Key)
		}

		line += fmt.Sprintf(
			" binding_fingerprint=%s channel_fingerprint=%s",
			fingerprintPrefix(binding.KeyFingerprint),
			fingerprintPrefix(channelFingerprint),
		)
	}

	if cause != nil {
		line += fmt.Sprintf(" cause=%q", cause.Error())
	}

	common.GetLogger(c).Warn(line)

	return nativetask.Plan{}, nil, &nativeRouteError{reason: reason}
}

func fingerprintPrefix(fingerprint string) string {
	if fingerprint == "" {
		return "none"
	}

	if len(fingerprint) > 8 {
		return fingerprint[:8]
	}

	return fingerprint
}
