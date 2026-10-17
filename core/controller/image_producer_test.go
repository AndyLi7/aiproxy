package controller

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func installControllerImageProducer(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, folder := range []string{"channel-execution-v1", "channel-image-producer-v1"} {
		raw, e := os.ReadFile("../migrations/" + folder + "/sqlite.sql")
		require.NoError(t, e)
		var lines []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				lines = append(lines, line)
			}
		}
		parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, p := range parts[1 : len(parts)-2] {
				if e := tx.Exec(strings.TrimSpace(p)).Error; e != nil {
					return e
				}
			}
			return nil
		}))
	}
}
func controllerProducerRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionImageProducer {
	t.Helper()
	var rows []model.ChannelAdmissionImageProducer
	require.NoError(t, db.Order("attempt_index").Find(&rows).Error)
	return rows
}
func TestImageProducerActualHTTP(t *testing.T) {
	for _, scenario := range []string{"queued", "receipt-failure", "return-failure", "cleanup-error", "cleanup-panic", "cleanup-goexit", "refused", "release-return-failure", "late", "partial"} {
		t.Run(scenario, func(t *testing.T) {
			status := http.StatusOK
			if scenario == "refused" || scenario == "release-return-failure" {
				status = http.StatusPaymentRequired
			}
			h := newImageCallLogHarness(t, status)
			activateImageAdmission(t, h)
			if scenario != "late" {
				installControllerImageProducer(t, h.db)
			}
			if scenario == "partial" {
				require.NoError(t, h.db.Exec("ALTER TABLE channel_admission_image_producers DROP COLUMN core_hash").Error)
			}
			walletSeen, providerSeen := false, false
			h.walletHook = func(c balance.PrepaymentCommand) {
				walletSeen = true
				if scenario == "late" {
					if c.Action == "admit" {
						installControllerImageProducer(t, h.db)
					}
					return
				}
				rows := controllerProducerRows(t, h.db)
				require.NotEmpty(t, rows)
				require.Equal(t, "active", rows[0].State)
				require.Zero(t, rows[0].ReturnedAtMs)
				if c.Action == "admit" {
					require.Len(t, rows, 1)
				}
			}
			h.providerHook = func() {
				providerSeen = true
				if scenario != "late" {
					rows := controllerProducerRows(t, h.db)
					require.Len(t, rows, 2)
					for _, r := range rows {
						require.Equal(t, "active", r.State)
					}
				}
			}
			old := updateImageTaskCallLog
			t.Cleanup(func() { updateImageTaskCallLog = old })
			if strings.HasPrefix(scenario, "cleanup-") {
				updateImageTaskCallLog = func(logID int, requestID, groupID string, status int, stage model.FailureStage, errorCode, safeError string) error {
					switch scenario {
					case "cleanup-panic":
						panic("synthetic cleanup panic")
					case "cleanup-goexit":
						runtime.Goexit()
					}
					return errors.New("synthetic cleanup failure")
				}
			}
			require.NoError(t, h.db.Callback().Update().Before("gorm:update").Register("producer:failure", func(tx *gorm.DB) {
				m, ok := tx.Statement.Dest.(map[string]any)
				if !ok {
					return
				}
				if (scenario == "receipt-failure" || strings.HasPrefix(scenario, "cleanup-")) && tx.Statement.Table == "image_tasks" && m["status"] == "queued" {
					tx.AddError(errors.New("synthetic receipt failure"))
				}
				if (scenario == "return-failure" || scenario == "release-return-failure") && tx.Statement.Table == "channel_admission_image_producers" && m["returned_at_ms"] != nil {
					tx.AddError(errors.New("synthetic return failure"))
				}
			}))
			if scenario == "cleanup-panic" {
				require.Panics(t, func() { h.submit() })
			} else if scenario == "cleanup-goexit" {
				done := make(chan struct{})
				go func() { defer close(done); h.submit() }()
				<-done
			} else {
				response := h.submit()
				if scenario == "partial" {
					require.Equal(t, 503, response.Code)
				} else if scenario == "refused" || scenario == "release-return-failure" {
					require.Equal(t, 402, response.Code)
				} else if scenario == "queued" || scenario == "late" || scenario == "return-failure" {
					require.Equal(t, 202, response.Code)
				}
			}
			if scenario == "partial" {
				require.False(t, walletSeen)
				require.False(t, providerSeen)
				return
			}
			rows := controllerProducerRows(t, h.db)
			if scenario == "late" {
				require.Empty(t, rows)
				return
			}
			require.True(t, walletSeen)
			active := strings.HasPrefix(scenario, "cleanup-") || scenario == "return-failure" || scenario == "release-return-failure"
			for _, r := range rows {
				if active {
					require.Equal(t, "active", r.State)
					require.Zero(t, r.ReturnedAtMs)
				} else {
					require.NotEqual(t, "active", r.State)
					require.GreaterOrEqual(t, r.ReturnedAtMs, r.StartedAtMs)
				}
				require.Equal(t, "unproven", r.ContinuationState)
			}
			uses := imageAdmissionUses(t, h)
			for _, u := range uses {
				if scenario == "refused" {
					require.Equal(t, "closed", u.State)
				} else {
					require.Equal(t, "open", u.State)
				}
			}
			if scenario == "queued" || scenario == "receipt-failure" {
				before := len(rows)
				commands, calls := len(h.commands), h.providerCalls
				require.Equal(t, 202, h.submit().Code)
				require.Equal(t, before, len(controllerProducerRows(t, h.db)))
				require.Equal(t, calls, h.providerCalls)
				require.Equal(t, commands, len(h.commands))
			}
		})
	}
}

type producerRoundTrip func(*http.Request) (*http.Response, error)

func (f producerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type producerCloseBody struct {
	io.ReadCloser
	closeHook func()
}

func (b producerCloseBody) Close() error { e := b.ReadCloser.Close(); b.closeHook(); return e }

func TestImageProducerActualBodyCloseBoundary(t *testing.T) {
	for _, panicClose := range []bool{false, true} {
		t.Run(fmt.Sprint(panicClose), func(t *testing.T) {
			h := newImageCallLogHarness(t, http.StatusOK)
			activateImageAdmission(t, h)
			installControllerImageProducer(t, h.db)
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			closed := false
			http.DefaultTransport = producerRoundTrip(func(r *http.Request) (*http.Response, error) {
				response, e := original.RoundTrip(r)
				if e == nil && strings.HasPrefix(r.URL.String(), h.channel.BaseURL) {
					response.Body = producerCloseBody{ReadCloser: response.Body, closeHook: func() {
						closed = true
						for _, row := range controllerProducerRows(t, h.db) {
							require.Equal(t, "active", row.State)
						}
						if panicClose {
							panic("synthetic provider Body.Close panic")
						}
					}}
				}
				return response, e
			})
			if panicClose {
				require.Panics(t, func() { h.submit() })
			} else {
				require.Equal(t, 202, h.submit().Code)
			}
			require.True(t, closed)
			for _, row := range controllerProducerRows(t, h.db) {
				if panicClose {
					require.Equal(t, "active", row.State)
				} else {
					require.Equal(t, "returned", row.State)
				}
			}
		})
	}
}
