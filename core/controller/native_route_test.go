//nolint:testpackage
package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

const (
	nativeRouteTestModel = "vendor/native-route"
	nativeRouteTestKey   = "fal-test-key"
	boundChannelID       = 11
	siblingChannelID     = 12
)

// nativeRouteFixture is the publish window: the bound channel S_old and a
// sibling S_new with the same key, both enabled and both listing the model.
// boundSet is the set the bound channel lists the model under ("" = none).
type nativeRouteFixture struct {
	bound, sibling *model.Channel
	boundSet       string
	binding        map[string]any
	quote          map[string]any
}

func newNativeRouteFixture() *nativeRouteFixture {
	return &nativeRouteFixture{
		bound: &model.Channel{
			ID: boundChannelID, Type: model.ChannelTypeFal, Key: nativeRouteTestKey,
			Status: model.ChannelStatusEnabled, Models: []string{nativeRouteTestModel},
		},
		sibling: &model.Channel{
			ID: siblingChannelID, Type: model.ChannelTypeFal, Key: nativeRouteTestKey,
			Status: model.ChannelStatusEnabled, Models: []string{nativeRouteTestModel},
		},
		boundSet: model.ChannelDefaultSet,
		binding: map[string]any{
			"version":         1,
			"channelId":       boundChannelID,
			"endpoint":        "fal-ai/native-route",
			"credentialScope": "scope",
			"keyFingerprint": model.ImageChannelKeyFingerprint(
				nativeRouteTestKey,
			),
			"contract": map[string]any{},
		},
		quote: map[string]any{
			"version": 1, "currency": "USD", "quoteVersion": "q1", "prepaidMicros": 1000,
			"routes": []any{
				map[string]any{
					"routeId":         "r1",
					"channelId":       boundChannelID,
					"provider":        "fal",
					"endpoint":        "fal-ai/native-route",
					"credentialScope": "scope",
					"estimatedMicros": 1000,
					"prepaidMicros":   1000,
					"rule":            map[string]any{"mode": "list_ratio", "ratio": "1"},
				},
			},
		},
	}
}

func (f *nativeRouteFixture) caches() *model.ModelCaches {
	bySet := map[string]map[string][]*model.Channel{}

	byID := map[int]*model.Channel{}
	for _, listed := range []struct {
		channel *model.Channel
		set     string
	}{{f.bound, f.boundSet}, {f.sibling, model.ChannelDefaultSet}} {
		byID[listed.channel.ID] = listed.channel
		if listed.channel.Status == model.ChannelStatusEnabled && listed.set != "" {
			if bySet[listed.set] == nil {
				bySet[listed.set] = map[string][]*model.Channel{}
			}

			bySet[listed.set][nativeRouteTestModel] = append(
				bySet[listed.set][nativeRouteTestModel],
				listed.channel,
			)
		}
	}

	return &model.ModelCaches{
		ChannelsByID:               byID,
		EnabledModel2ChannelsBySet: bySet,
		Version:                    42,
	}
}

func (f *nativeRouteFixture) config() model.ModelConfig {
	values := map[model.ModelConfigKey]any{}
	if f.binding != nil {
		values[NativeResultConfigKey] = f.binding
	}

	if f.quote != nil {
		values[model.ImagePrepaymentConfigKey] = f.quote
	}

	return model.ModelConfig{Model: nativeRouteTestModel, Type: mode.NativeTasks, Config: values}
}

// context is a submission after the distributor; header names a channel with
// Aiproxy-Channel (internal groups only).
func (f *nativeRouteFixture) context(t *testing.T, header string) *gin.Context {
	t.Helper()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/model-tasks", nil)

	group := model.GroupCache{ID: "g", Status: model.GroupStatusEnabled}
	if header != "" {
		group.Status = model.GroupStatusInternal

		c.Request.Header.Set(AIProxyChannelHeader, header)
	}

	c.Set(middleware.Group, group)
	c.Set(middleware.Token, model.TokenCache{ID: 7})
	c.Set(middleware.ModelCaches, f.caches())
	c.Set(middleware.ModelConfig, f.config())
	c.Set(middleware.RoutingModel, nativeRouteTestModel)

	return c
}

func setNativeRouteFollowsBinding(t *testing.T, on bool) {
	t.Helper()

	previous := config.NativeRouteFollowsBinding
	config.NativeRouteFollowsBinding = on
	t.Cleanup(func() { config.NativeRouteFollowsBinding = previous })
}

func captureNativeRouteLogs(t *testing.T) *logtest.Hook {
	t.Helper()

	hook := new(logtest.Hook)
	oldHooks := logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))

	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(oldHooks) })

	return hook
}

func nativeRouteReason(t *testing.T, err error) string {
	t.Helper()

	var rejection *nativeRouteError
	require.True(t, errors.As(err, &rejection), "want a native route rejection, got %v", err)

	return rejection.reason
}

// The publish-window incident: two enabled channels serve every fal model and
// selection used to pick either before comparing with the binding.
func TestNativeRouteFollowsBindingDuringPublishWindow(t *testing.T) {
	setNativeRouteFollowsBinding(t, true)

	fixture := newNativeRouteFixture()

	for range 1000 {
		plan, provider, err := resolveNativePlan(fixture.context(t, ""))
		require.NoError(t, err)
		require.NotNil(t, provider)
		require.Equal(t, boundChannelID, plan.ChannelID)

		identity, identityErr := model.ChannelIdentityOf(fixture.bound)
		require.NoError(t, identityErr)
		require.Equal(t, identity.IdentityHash, plan.AdmissionIdentityHash)
	}

	// A higher-priority sibling still loses and a backup-only bound channel is
	// still picked: the filter alone leaves the bound channel as the only
	// candidate, and selection falls back to backup-only candidates.
	fixture.sibling.Priority, fixture.bound.BackupOnly = 1000, true
	for range 100 {
		selected, err := getInitialChannel(
			fixture.context(t, ""),
			nativeRouteTestModel,
			mode.NativeTasks,
		)
		require.NoError(t, err)
		require.Equal(t, boundChannelID, selected.channel.ID)
		require.Len(t, selected.migratedChannels, 1)
	}
}

// NATIVE_ROUTE_FOLLOWS_BINDING=false restores the old selection: the sibling
// is picked too and then refused by the identity check.
func TestNativeRouteFollowsBindingSwitchOff(t *testing.T) {
	setNativeRouteFollowsBinding(t, false)

	fixture := newNativeRouteFixture()

	accepted, rejected := 0, 0
	for range 1000 {
		plan, _, err := resolveNativePlan(fixture.context(t, ""))
		if err != nil {
			require.Equal(t, nativeRouteChannelIdentityMismatch, nativeRouteReason(t, err))

			rejected++
			continue
		}

		require.Equal(t, boundChannelID, plan.ChannelID)

		accepted++
	}

	require.Positive(t, accepted)
	require.Positive(t, rejected)
}

// An invalid binding admits no channel: the filter never degrades to "none".
func TestNativeBindingFilterRejectsEveryChannelWithoutBinding(t *testing.T) {
	setNativeRouteFollowsBinding(t, true)

	for name, binding := range map[string]map[string]any{
		"missing":     nil,
		"unbound":     {"version": 1, "channelId": 0},
		"old version": {"version": 2, "channelId": boundChannelID},
		"unparsable":  {"version": 1, "channelId": "eleven"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newNativeRouteFixture()
			fixture.binding = binding
			c := fixture.context(t, "")
			accept := nativeBindingFilter(c, mode.NativeTasks)
			require.NotNil(t, accept)
			require.False(t, accept(fixture.bound))
			require.False(t, accept(fixture.sibling))

			_, err := getInitialChannel(c, nativeRouteTestModel, mode.NativeTasks)
			require.ErrorIs(t, err, ErrChannelsNotFound)
		})
	}

	// Other modes and the switched-off state keep their old selection.
	c := newNativeRouteFixture().context(t, "")
	require.Nil(t, nativeBindingFilter(c, mode.ImagesGenerations))
	setNativeRouteFollowsBinding(t, false)
	require.Nil(t, nativeBindingFilter(c, mode.NativeTasks))
}

// Every ResolvePlan failure names its reason in the log, with the binding,
// the selected channel and the cache version, and never the key.
func TestNativeRouteRejectionReasons(t *testing.T) {
	setNativeRouteFollowsBinding(t, true)

	fingerprint := model.ImageChannelKeyFingerprint(nativeRouteTestKey)

	for _, tc := range []struct {
		name, reason, selected string
		change                 func(*nativeRouteFixture)
	}{
		{
			name: "unbound placeholder", reason: nativeRouteBindingInvalid, selected: "selected_channel=0",
			change: func(f *nativeRouteFixture) { f.binding["channelId"] = 0 },
		},
		{
			name: "unparsable binding", reason: nativeRouteBindingInvalid, selected: "selected_channel=0",
			change: func(f *nativeRouteFixture) { f.binding["channelId"] = "eleven" },
		},
		{
			name: "bound channel disabled", reason: nativeRouteBoundChannelUnavailable, selected: "selected_channel=0",
			change: func(f *nativeRouteFixture) { f.bound.Status = model.ChannelStatusDisabled },
		},
		{
			name: "bound channel not listing the model", reason: nativeRouteBoundChannelUnavailable, selected: "selected_channel=0",
			change: func(f *nativeRouteFixture) { f.bound.Models, f.boundSet = nil, "" },
		},
		{
			name: "bound channel outside the group's sets", reason: nativeRouteBoundChannelUnavailable, selected: "selected_channel=0",
			change: func(f *nativeRouteFixture) { f.boundSet = "other-set" },
		},
		{
			name: "base url", reason: nativeRouteChannelIdentityMismatch, selected: "selected_channel=11",
			change: func(f *nativeRouteFixture) { f.bound.BaseURL = "https://example.invalid" },
		},
		{
			name: "fingerprint", reason: nativeRouteKeyFingerprintMismatch, selected: "selected_channel=11",
			change: func(f *nativeRouteFixture) {
				f.binding["keyFingerprint"] = model.ImageChannelKeyFingerprint("other-key")
			},
		},
		{
			name: "empty key", reason: nativeRouteKeyFingerprintMismatch, selected: "selected_channel=11",
			change: func(f *nativeRouteFixture) { f.bound.Key = "" },
		},
		{
			name: "missing quote", reason: nativeRouteQuoteInvalid, selected: "selected_channel=11",
			change: func(f *nativeRouteFixture) { f.quote = nil },
		},
		{
			name: "invalid quote", reason: nativeRouteQuoteInvalid, selected: "selected_channel=11",
			change: func(f *nativeRouteFixture) { f.quote["currency"] = "EUR" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := captureNativeRouteLogs(t)
			fixture := newNativeRouteFixture()
			tc.change(fixture)
			_, provider, err := resolveNativePlan(fixture.context(t, ""))
			require.Nil(t, provider)
			require.Equal(t, tc.reason, nativeRouteReason(t, err))

			entry := hook.LastEntry()
			require.NotNil(t, entry)
			require.Equal(t, logrus.WarnLevel, entry.Level)
			require.Contains(t, entry.Message, "reason="+tc.reason+" ")
			require.Contains(t, entry.Message, "route="+nativeRouteTestModel)
			require.Contains(t, entry.Message, tc.selected)
			require.Contains(t, entry.Message, "cache_version=42")
			require.NotContains(t, entry.Message, nativeRouteTestKey)
			require.NotContains(t, entry.Message, fingerprint)

			if tc.reason == nativeRouteKeyFingerprintMismatch {
				require.Contains(t, entry.Message, "binding_fingerprint=")
			}
		})
	}

	// The fingerprint line carries 8-character prefixes only.
	hook := captureNativeRouteLogs(t)
	fixture := newNativeRouteFixture()
	other := model.ImageChannelKeyFingerprint("other-key")
	fixture.binding["keyFingerprint"] = other
	_, _, err := resolveNativePlan(fixture.context(t, ""))
	require.Error(t, err)

	message := hook.LastEntry().Message
	require.Contains(
		t,
		message,
		"binding_fingerprint="+other[:8]+" channel_fingerprint="+fingerprint[:8],
	)
	require.NotContains(t, message, other[:9])
	require.NotContains(t, message, fingerprint[:9])
}

// An Aiproxy-Channel header or a pinned channel naming another channel than
// the binding is refused; naming the bound channel works while it is enabled.
func TestNativeRouteHeaderMustNameBoundChannel(t *testing.T) {
	setNativeRouteFollowsBinding(t, true)

	fixture := newNativeRouteFixture()

	_, _, err := resolveNativePlan(fixture.context(t, strconv.Itoa(siblingChannelID)))
	require.Equal(t, nativeRouteBoundChannelUnavailable, nativeRouteReason(t, err))
	_, err = getInitialChannel(
		fixture.context(t, strconv.Itoa(siblingChannelID)),
		nativeRouteTestModel,
		mode.NativeTasks,
	)
	require.ErrorIs(t, err, ErrChannelsNotFound)

	plan, _, err := resolveNativePlan(fixture.context(t, strconv.Itoa(boundChannelID)))
	require.NoError(t, err)
	require.Equal(t, boundChannelID, plan.ChannelID)

	pinned := fixture.context(t, "")
	pinned.Set(middleware.ChannelID, siblingChannelID)
	_, _, err = resolveNativePlan(pinned)
	require.Equal(t, nativeRouteBoundChannelUnavailable, nativeRouteReason(t, err))

	// The header reads every cached channel, disabled ones included: a bound
	// channel the publisher has not enabled yet (or has retired) is refused.
	disabled := newNativeRouteFixture()
	disabled.bound.Status = model.ChannelStatusDisabled
	_, _, err = resolveNativePlan(disabled.context(t, strconv.Itoa(boundChannelID)))
	require.Equal(t, nativeRouteBoundChannelUnavailable, nativeRouteReason(t, err))

	// Switched off, the header is honoured and the identity check refuses it.
	setNativeRouteFollowsBinding(t, false)
	_, _, err = resolveNativePlan(fixture.context(t, strconv.Itoa(siblingChannelID)))
	require.Equal(t, nativeRouteChannelIdentityMismatch, nativeRouteReason(t, err))
}

// Through the handler the customer still gets 503 model_route_unavailable,
// nothing is reserved, and the log names the cause.
func TestNativeSubmissionToDisabledBoundChannelIs503(t *testing.T) {
	setNativeRouteFollowsBinding(t, true)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))

	oldDB, oldWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = oldDB; balance.Default = oldWallet })

	model.LogDB, balance.Default = db, nativeControllerWallet{}
	hook := captureNativeRouteLogs(t)

	fixture := newNativeRouteFixture()
	fixture.bound.Status = model.ChannelStatusDisabled
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.Group, model.GroupCache{ID: "g", Status: model.GroupStatusEnabled})
		c.Set(middleware.Token, model.TokenCache{ID: 7})
		c.Set(middleware.ModelCaches, fixture.caches())
		c.Set(middleware.ModelConfig, fixture.config())
		c.Set(middleware.RoutingModel, nativeRouteTestModel)
		middleware.SetRequestID(c, "native-route-test")
		c.Next()
	})
	router.POST("/v1/model-tasks", submitNativeTask)

	request := httptest.NewRequestWithContext(t.Context(),
		http.MethodPost,
		"/v1/model-tasks",
		strings.NewReader(`{"model":"`+nativeRouteTestModel+`","input":{}}`),
	)
	request.Header.Set("X-Request-ID", "native-route-test")

	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())

	var body nativeErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, "model_route_unavailable", body.Error.Code)
	require.NotContains(t, response.Body.String(), "bound_channel")

	var reserved int64
	require.NoError(t, db.Model(&model.NativeTask{}).Count(&reserved).Error)
	require.Zero(t, reserved)

	var logged string
	for _, entry := range hook.AllEntries() {
		if strings.HasPrefix(entry.Message, "native route rejected") {
			logged = entry.Message
		}
	}

	require.Contains(
		t,
		logged,
		"reason=bound_channel_unavailable request_id=native-route-test route="+nativeRouteTestModel+" binding_channel=11 selected_channel=0 cache_version=42",
	)
}
