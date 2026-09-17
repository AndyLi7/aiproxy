package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/stretchr/testify/require"
)

func TestImageTaskFingerprintCanonical(t *testing.T) {
	a, err := imageTaskFingerprint([]byte(`{"model":"m","prompt":"x","n":1}`))
	require.NoError(t, err)
	b, err := imageTaskFingerprint([]byte(`{"n":1,"prompt":"x","model":"m"}`))
	require.NoError(t, err)
	require.Equal(t, a, b)

	c, _ := imageTaskFingerprint([]byte(`{"n":1,"prompt":"y","model":"m"}`))
	require.NotEqual(t, a, c)
}

func TestImageTaskHTTPReplayConflictAndOwner(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "http.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	body := `{"model":"public","prompt":"test","n":1}`
	fp, _ := imageTaskFingerprint([]byte(body))
	_, _, err = model.ReserveImageTask(
		&model.ImageTask{
			ID:          "request-a",
			Model:       "public",
			GroupID:     "g",
			TokenID:     1,
			Fingerprint: fp,
		},
		&model.AsyncUsageInfo{},
	)
	require.NoError(t, err)
	require.NoError(t, model.SetImageTaskResult("request-a", "submission_unknown", nil, nil))

	for _, tc := range []struct {
		method, body  string
		token, status int
	}{
		{"POST", body, 1, 202}, {"POST", `{"model":"public","prompt":"different"}`, 1, 409}, {"GET", "", 1, 200}, {"GET", "", 2, 404},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequestWithContext(t.Context(),
			tc.method,
			"/v1/images/tasks/request-a",
			strings.NewReader(tc.body),
		)
		c.Request.Header.Set("X-Request-ID", "request-a")
		c.Params = gin.Params{{Key: "id", Value: "request-a"}}
		c.Set(middleware.Group, model.GroupCache{ID: "g"})
		c.Set(middleware.Token, model.TokenCache{ID: tc.token})
		c.Set(middleware.RequestModel, "public")
		c.Set(
			middleware.ModelConfig,
			model.ModelConfig{
				Config: map[model.ModelConfigKey]any{
					"x_token_platform_capability_contract": map[string]any{
						"contract": map[string]any{
							"execution": map[string]any{"mode": "async", "output": "image"},
						},
					},
				},
			},
		)

		if tc.method == "POST" {
			submitImageTask(c)
		} else {
			GetImageTask(c)
		}

		require.Equal(t, tc.status, w.Code)
		require.NotContains(t, w.Body.String(), "fingerprint")

		if tc.status == 202 {
			require.Contains(t, w.Body.String(), "submission_unknown")
		}
	}
}

func TestImageReplayDoesNotRequireActivePricingOrChannel(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "replay.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	contract := `{"entry_id":"public","validation_version":1,"input_schema":{"type":"object","properties":{"prompt":{"type":"string"},"n":{"type":"integer","default":1}},"required":["prompt"],"additionalProperties":false}}`
	fp, _ := imageTaskFingerprint([]byte(`{"model":"public","prompt":"test","n":1}`))
	require.NoError(
		t,
		db.Create(
			&model.ImageTask{
				ID:                 "replay-id",
				Model:              "public",
				RequestModel:       "public",
				GroupID:            "g",
				TokenID:            1,
				Fingerprint:        fp,
				Status:             "completed",
				ValidationContract: contract,
			},
		).Error,
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(t.Context(),
		http.MethodPost,
		"/v1/images/tasks",
		strings.NewReader(`{"model":"public","prompt":"test"}`),
	)
	c.Request.Header.Set("X-Request-ID", "replay-id")
	c.Set(middleware.Group, model.GroupCache{ID: "g"})
	c.Set(middleware.Token, model.TokenCache{ID: 1})
	replayImageTask(c)
	require.Equal(t, 202, w.Code)
	require.True(t, c.IsAborted())
	require.Contains(t, w.Body.String(), "completed")
}

func TestImageSubmitWithCompiledRegistryFixture(t *testing.T) {
	fixture, err := os.ReadFile("../testdata/fal-minimax-contract.json")
	require.NoError(t, err)

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(fixture, &metadata))
	contract, err := json.Marshal(metadata["contract"])
	require.NoError(t, err)

	const public = "minimax/image-01/text-to-image"

	normalized, validationErr := registryvalidation.ValidateImage(
		contract,
		public,
		[]byte(`{"model":"minimax/image-01/text-to-image","prompt":"test"}`),
	)
	require.Nil(t, validationErr)

	for _, tc := range []struct {
		name              string
		upstreamCode      int
		price             model.Price
		balance, required float64
		status            int
	}{
		{"accepted", 200, model.Price{}, 1, 0.01, 202},
		{"rejected", 422, model.Price{}, 1, 0.01, 202},
		{"unknown", 503, model.Price{}, 1, 0.01, 202},
		{"image insufficient", 200, model.Price{ImageOutputPrice: 0.25, ImageOutputPriceUnit: 1}, 0.1, 0.25, 403},
		{"image sufficient", 200, model.Price{ImageOutputPrice: 0.25, ImageOutputPriceUnit: 1}, 0.25, 0.25, 202},
		{"request insufficient", 200, model.Price{PerRequestPrice: 0.5}, 0.1, 0.5, 403},
		{"request sufficient", 200, model.Price{PerRequestPrice: 0.5}, 0.5, 0.5, 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GROUP_MINIMUM_BALANCE", "0.01")

			code := tc.upstreamCode
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "submit.db"))
			require.NoError(t, err)
			require.NoError(
				t,
				db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}),
			)

			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })

			calls := 0

			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++

				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(
					t,
					map[string]any{
						"prompt":           "test",
						"num_images":       float64(1),
						"aspect_ratio":     "1:1",
						"prompt_optimizer": false,
					},
					body,
				)

				var info model.AsyncUsageInfo
				require.NoError(t, db.First(&info).Error)
				require.NotZero(t, info.LogID)
				require.Equal(t, model.AsyncUsageStatusNone, info.Status)
				w.WriteHeader(code)

				if code == 200 {
					if _, writeErr := w.Write(
						[]byte(`{"request_id":"upstream"}`),
					); writeErr != nil {
						t.Errorf("write mock response: %v", writeErr)
					}
				}
			}))
			defer s.Close()

			ch := &model.Channel{
				ID:           7,
				Type:         model.ChannelTypeFal,
				Key:          "secret",
				BaseURL:      s.URL,
				Models:       []string{public},
				ModelMapping: map[string]string{public: "fal-ai/minimax/image-01"},
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequestWithContext(t.Context(),
				http.MethodPost,
				"/v1/images/tasks",
				bytes.NewReader(normalized),
			)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("X-Request-ID", "fixture-request")
			c.Request.Header.Set(AIProxyChannelHeader, "7")
			c.Set(middleware.Group, model.GroupCache{ID: "g", Status: model.GroupStatusInternal})
			c.Set(middleware.Token, model.TokenCache{ID: 1})
			c.Set(middleware.RequestModel, "minimax/image-01")
			c.Set(middleware.RoutingModel, public)
			c.Set(middleware.RequestedModel, public)
			c.Set(
				middleware.ModelCaches,
				&model.ModelCaches{ChannelsByID: map[int]*model.Channel{7: ch}},
			)
			c.Set(
				middleware.ModelConfig,
				model.ModelConfig{
					Price: tc.price,
					Config: map[model.ModelConfigKey]any{
						"x_token_platform_capability_contract": metadata,
						"x_token_platform_pricing": map[string]any{
							"currency":        "USD",
							"pricing_version": "fixture-v1",
						},
					},
				},
			)

			balanceChecks := 0
			c.Set(middleware.GroupBalance, &middleware.GroupBalanceConsumer{
				Group: "g",
				CheckBalance: func(required float64) bool {
					balanceChecks++

					require.InDelta(t, tc.required, required, 0.000001)
					return tc.balance >= required
				},
			})
			submitImageTask(c)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, 1, balanceChecks)

			if tc.status == 403 {
				require.Zero(t, calls)

				for _, table := range []any{&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}} {
					var count int64
					require.NoError(t, db.Model(table).Count(&count).Error)
					require.Zero(t, count)
				}

				return
			}

			require.Equal(t, 1, calls)

			var result model.ImageTask
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))

			expected := map[int]string{200: "queued", 422: "failed", 503: "submission_unknown"}[code]
			require.Equal(t, expected, result.Status)
			require.Equal(t, public, result.Model)
		})
	}
}

type syncTaskFake struct {
	calls  int
	err    error
	result adaptor.ImageTaskResult
}

func (*syncTaskFake) ImageAdapterName() string { return "volcengine-ark-image" }
func (a *syncTaskFake) GenerateImage(context.Context, *meta.Meta, []byte, []byte) (adaptor.ImageTaskResult, error) {
	a.calls++
	return a.result, a.err
}
func TestSyncDispatchPersistsOrQuarantinesWithoutResubmission(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		err          error
		data         []model.ImageOutput
	}{
		{name: "success", status: "completed", data: []model.ImageOutput{{URL: "https://example.com/a"}}},
		{name: "unknown", status: "submission_unknown", err: errors.New("timeout")},
		{name: "rejected", status: "failed", err: adaptor.ErrImageSubmissionRejected},
		{name: "too many outputs", status: "failed", data: []model.ImageOutput{{URL: "https://example.com/a"}, {URL: "https://example.com/b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sync-dispatch.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			original := &model.ImageTask{ID: "sync-test", Model: "m", GroupID: "g", TokenID: 1, Fingerprint: "f", ExpectedImages: 1}
			saved, created, err := model.ReserveImageTask(original, &model.AsyncUsageInfo{RequestID: original.ID})
			require.NoError(t, err)
			require.True(t, created)
			a := &syncTaskFake{err: tc.err, result: adaptor.ImageTaskResult{Status: "completed", Data: tc.data}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/images/tasks", nil)
			dispatchSyncImageTask(c, t.Context(), saved, a, &meta.Meta{}, nil)
			got, err := model.GetImageTask(original.ID, "g", 1)
			require.NoError(t, err)
			require.Equal(t, tc.status, got.Status)
			_, created, err = model.ReserveImageTask(&model.ImageTask{ID: original.ID, Model: "m", GroupID: "g", TokenID: 1, Fingerprint: "f"}, &model.AsyncUsageInfo{})
			require.NoError(t, err)
			require.False(t, created)
			require.Equal(t, 1, a.calls)
			var info model.AsyncUsageInfo
			require.NoError(t, db.First(&info).Error)
			switch tc.status {
			case "completed":
				require.Equal(t, model.AsyncUsageStatusPending, info.Status)
			case "submission_unknown":
				require.Equal(t, model.AsyncUsageStatusNone, info.Status)
			default:
				require.Equal(t, model.AsyncUsageStatusFailed, info.Status)
			}
		})
	}
}
