//nolint:testpackage // White-box coverage needs the package-private newRelayAdmissionHarness fixture or implementation.
package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func installControllerMeasured(t *testing.T, db *gorm.DB) {
	t.Helper()

	for _, folder := range []string{"channel-execution-v1", "channel-measured-handoff-v1"} {
		raw, e := os.ReadFile("../migrations/" + folder + "/sqlite.sql")
		require.NoError(t, e)

		var lines []string
		for l := range strings.SplitSeq(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				lines = append(lines, l)
			}
		}

		parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, p := range parts[1 : len(parts)-2] {
				if e := tx.Exec(p).Error; e != nil {
					return e
				}
			}

			return nil
		}))
	}

	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}, &model.RetryLog{}))
}

func TestMeasuredHandoffActualRelay(t *testing.T) {
	for _, scenario := range []string{"normal", "outbox-error", "outer-panic", "return-error", "late", "partial", "two-attempts", "wrong-attempt", "lost-handle", "missing-unused-column"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRelayAdmissionHarness(t)
			if scenario != "late" {
				installControllerMeasured(t, h.db)
			} else {
				require.NoError(t, h.db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))
			}

			if scenario == "missing-unused-column" {
				require.NoError(
					t,
					h.db.Exec(
						"ALTER TABLE channel_admission_measured_claims DROP COLUMN release_hash",
					).Error,
				)
				require.False(
					t,
					h.db.Migrator().
						HasColumn(&model.ChannelAdmissionMeasuredClaim{}, "release_hash"),
				)
			}

			if scenario == "partial" {
				require.NoError(
					t,
					h.db.Exec(
						"ALTER TABLE channel_admission_measured_producers DROP COLUMN frozen_hash",
					).Error,
				)
			}

			h.responseHook = func(w http.ResponseWriter, _ *http.Request) {
				if scenario == "late" {
					installControllerMeasured(t, h.db)
				} else {
					var row model.ChannelAdmissionMeasuredProducer
					require.NoError(t, h.db.Where("state=?", "active").First(&row).Error)
					require.Equal(t, "active", row.State)
					require.Zero(t, row.UsageID)
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte(
						`{"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=","size":"2x2"}],"usage":{"input_images":0,"generated_images":1}}`,
					),
				)
			}
			price := model.Price{
				OutputPrice:     .3,
				OutputPriceUnit: 1,
				ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
			}
			call := func(channel int) (*gin.Context, *meta.Meta, *relaycontroller.HandleResult) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequestWithContext(
					t.Context(),
					http.MethodPost,
					"/v1/images/generations",
					strings.NewReader(`{"model":"synthetic","prompt":"fixture"}`),
				)
				c.Request.Header.Set("Content-Type", "application/json")

				m := meta.NewMeta(
					h.channels[channel],
					mode.ImagesGenerations,
					"synthetic",
					model.ModelConfig{Price: price},
					meta.WithRequestID("measured-http"),
					meta.WithBillingOperationID("measured-http-op"),
				)
				m.Group = model.GroupCache{ID: "g"}
				m.Token = model.TokenCache{ID: 1, Group: "g"}
				m.MeasuredRelay = model.NewMeasuredRelayIntent(price, m.RequestAt, false)
				result, _ := RelayHelper(
					c,
					m,
					func(c *gin.Context, m *meta.Meta) *relaycontroller.HandleResult {
						return relayHandler(c, m, &model.ModelCaches{})
					},
				)

				return c, m, result
			}

			c, m, result := call(0)
			if scenario == "partial" || scenario == "missing-unused-column" {
				require.NotNil(t, result.Error)
				require.Zero(t, h.calls.Load())
				return
			}

			require.Nil(t, result.Error)

			if scenario != "late" {
				var ex model.ChannelAdmissionExecution
				require.NoError(t, h.db.First(&ex).Error)
				require.Equal(t, "returned", ex.State)
			}

			if scenario == "outbox-error" || scenario == "outer-panic" {
				require.NoError(
					t,
					h.db.Callback().
						Create().
						Before("gorm:create").
						Register("measured:reject", func(tx *gorm.DB) {
							if tx.Statement.Table == "async_usage_infos" {
								if scenario == "outer-panic" {
									panic("synthetic outer persistence panic")
								}

								_ = tx.AddError(errors.New("synthetic outbox failure"))
							}
						}),
				)
			}

			if scenario == "return-error" {
				require.NoError(
					t,
					h.db.Callback().
						Update().
						Before("gorm:update").
						Register("measured:return", func(tx *gorm.DB) {
							v, _ := tx.Statement.Dest.(map[string]any)
							if tx.Statement.Table == "channel_admission_measured_producers" &&
								v["returned_at_ms"] != nil {
								_ = tx.AddError(errors.New("synthetic return failure"))
							}
						}),
				)
			}

			if scenario == "two-attempts" || scenario == "wrong-attempt" {
				recordResult(c, m, price, result, 0, false, nil)
				old := result.MeasuredProducer

				c, m, result = call(1)
				if scenario == "wrong-attempt" {
					result.MeasuredProducer = old
				}
			}

			if scenario == "lost-handle" {
				result.MeasuredProducer = nil
			}

			if scenario == "outer-panic" {
				require.Panics(t, func() { recordResult(c, m, price, result, 0, true, nil) })
			} else {
				recordResult(c, m, price, result, 0, true, nil)
			}

			var rows []model.ChannelAdmissionMeasuredProducer
			require.NoError(t, h.db.Order("started_at_ms,id").Find(&rows).Error)

			if scenario == "late" {
				require.Empty(t, rows)
				return
			}

			var infos []model.AsyncUsageInfo
			require.NoError(t, h.db.Order("id").Find(&infos).Error)

			switch scenario {
			case "outbox-error", "outer-panic", "lost-handle":
				require.Len(t, rows, 1)
				require.Equal(t, "active", rows[0].State)
				require.Empty(t, infos)
			case "return-error":
				require.Len(t, infos, 1)
				require.Equal(t, "active", rows[0].State)
				require.Equal(t, "final", rows[0].HandoffKind)
			case "wrong-attempt":
				require.Len(t, infos, 1)
				require.Zero(t, infos[0].LogID)
				require.Equal(t, "active", rows[1].State)
			case "two-attempts":
				require.Len(t, infos, 2)
				require.Zero(t, infos[0].LogID)
				require.NotZero(t, infos[1].LogID)
				require.Equal(t, "returned", rows[0].State)
				require.Equal(t, "returned", rows[1].State)
			default:
				require.Len(t, infos, 1)
				require.Equal(t, "returned", rows[0].State)
				require.Equal(t, "final", rows[0].HandoffKind)
			}
		})
	}
}
