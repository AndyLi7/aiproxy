//nolint:testpackage // White-box coverage needs the package-private probeHTTPFixture fixture or implementation.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const retentionPath = "/api/channel-migrations/operations/http-probe-op/retention"

func retentionRead(t *testing.T, f *probeHTTPFixture) model.MigrationRetentionResult {
	t.Helper()
	code, raw := f.call(t, "GET", retentionPath, nil, config.AdminKey)
	require.Equal(t, 200, code)

	var r struct {
		Data model.MigrationRetentionResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &r))
	require.NotContains(t, string(raw), "synthetic-preparation-old")
	require.NotContains(t, string(raw), "private-payload-that-must-not-leak")

	return r.Data
}

func retentionCommitted(t *testing.T, f *probeHTTPFixture) {
	t.Helper()
	b := prepareHTTPProbeForCutover(t, f)
	_, raw := f.call(
		t,
		"POST",
		"/api/channel-migrations/operations/http-probe-op/cutover",
		b,
		config.AdminKey,
	)
	require.Contains(t, string(raw), `"state":"committed"`)
}

func retentionSource(
	t *testing.T,
	r model.MigrationRetentionResult,
	kind string,
) model.MigrationRetentionSource {
	t.Helper()

	for _, s := range r.Executions {
		if s.Kind == kind {
			return s
		}
	}

	t.Fatalf("missing source %s", kind)

	return model.MigrationRetentionSource{}
}

// Historical source fixtures deliberately include unknown/old states. They test
// read classification, not private executor capability reconstruction.
func retentionUse(t *testing.T, f *probeHTTPFixture, n int) model.ChannelAdmissionUse {
	t.Helper()

	u := model.ChannelAdmissionUse{
		ID:           fmt.Sprintf("%064x", n),
		ResourceKey:  fmt.Sprintf("%064x", n+100),
		RequestHash:  strings.Repeat("c", 64),
		Epoch:        f.body.Command.Seal.Epoch,
		ChannelID:    f.body.Command.Seal.From.ChannelID,
		KeyDigest:    f.body.Command.Seal.From.KeyDigest,
		IdentityHash: f.body.Command.Seal.From.IdentityHash,
		Lane:         "relay",
		State:        "open",
		AdmittedAtMs: time.Now().Add(-time.Hour).UnixMilli(),
	}

	return u
}

func retentionHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	} // Fixture hashing must never silently hash empty bytes.

	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

func retentionUseHash(u model.ChannelAdmissionUse) string {
	return retentionHash(
		[]any{
			u.ID,
			u.ResourceKey,
			u.RequestHash,
			u.Epoch,
			u.ChannelID,
			u.KeyDigest,
			u.IdentityHash,
			u.Lane,
			u.AdmittedAtMs,
		},
	)
}

func TestMigrationRetentionEmptyIsNotDrained(t *testing.T) {
	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)
	r := retentionRead(t, f)
	require.Equal(t, "ready", r.State)
	require.Equal(t, "committed_current", r.Cutover)
	require.Equal(t, "unproven", r.Retention)
	require.Equal(t, "unproven", r.HistoryCoverage)
	require.Equal(t, "not_observed_here", r.CacheSwitch)
	require.Equal(t, "not_evaluated_by_diagnostic", r.Authority)
	require.Equal(t, "0", r.Uses.ExactIdentityOpen)
	require.Equal(t, "0", r.Native.Recorded) // probe is on target, not old channel
	require.Contains(t, r.Reasons, "existing_barrier_or_use_management_guard")
	s := retentionSource(t, r, "image_poll")
	require.Equal(t, "unavailable", s.State)
	require.Nil(t, s.Counts)
	require.Equal(t, 1, f.providerCalls)
}

func TestMigrationRetentionRecordedHistoryAndNativeUnknown(t *testing.T) {
	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)
	u := retentionUse(t, f, 1)
	require.NoError(t, f.db.Create(&u).Error)
	active := model.ChannelAdmissionExecution{
		ID:                strings.Repeat("a", 64),
		UseID:             u.ID,
		UseHash:           retentionUseHash(u),
		Version:           1,
		Epoch:             u.Epoch,
		StorageGeneration: "probe-http-storage",
		State:             "active",
		ContinuationState: "unproven",
		StartedAtMs:       u.AdmittedAtMs,
	}
	require.NoError(t, f.db.Create(&active).Error)
	old := retentionUse(t, f, 2)
	old.Epoch = "previous-epoch"
	old.IdentityHash = strings.Repeat("b", 64)
	require.NoError(t, f.db.Create(&old).Error)

	returned := active
	returned.ID = strings.Repeat("b", 64)
	returned.UseID = old.ID
	returned.UseHash = retentionUseHash(old)
	returned.Epoch = old.Epoch
	returned.State = "returned"
	returned.ReturnedAtMs = returned.StartedAtMs + 1
	require.NoError(t, f.db.Create(&returned).Error)
	other := retentionUse(t, f, 3)
	other.ChannelID = 999
	require.NoError(t, f.db.Create(&other).Error) // same key, unrelated physical channel

	expired := time.Now().Add(-time.Hour)
	for i, status := range []string{"submission_unknown", "failed", "completed"} {
		task := model.NativeTask{
			ID:             fmt.Sprintf("old-task-%d", i),
			ChannelID:      u.ChannelID,
			GroupID:        "g",
			TokenID:        1,
			Fingerprint:    u.RequestHash,
			KeyFingerprint: u.KeyDigest,
			Status:         status,
			RecoveryOwner:  "crashed-owner",
			RecoveryUntil:  expired.Unix(),
			DispatchOwner:  "expired-dispatch",
			DispatchUntil:  &expired,
		}
		require.NoError(t, f.db.Create(&task).Error)
		use := retentionUse(t, f, 10+i)
		use.Lane = "native"
		use.ResourceKey = retentionHash([]any{"native", task.GroupID, task.TokenID, task.ID})
		require.NoError(t, f.db.Create(&use).Error)
	}

	r := retentionRead(t, f)
	require.Equal(t, "ready", r.State)
	require.Equal(t, "required", r.Retention)
	require.Equal(t, "4", r.Uses.ExactIdentityOpen)
	require.Equal(t, "1", r.Uses.OtherHistoricalIdentityOpen)
	require.Equal(t, "1", r.Uses.OldEpochRecorded)
	c := retentionSource(t, r, "relay").Counts
	require.Equal(t, "1", c.Active)
	require.Equal(t, "1", c.Returned)
	require.Equal(t, "2", c.ContinuationUnproven)
	require.Equal(t, "1", c.HistoricalGeneration)
	require.Equal(t, "0", c.InvalidBindingOrState)
	require.Equal(t, "1", r.Native.Nonterminal)
	require.Equal(t, "1", r.Native.UnknownAcceptance)
	require.Equal(t, "1", r.Native.TerminalAcceptanceUnproven)
	require.Equal(t, "3", r.Native.OwnerPresent)
	require.Equal(t, "3", r.Native.ExpiredOwnerPresent)
	require.Equal(t, "3", r.Native.LocalBillingUnsettled)
	require.Equal(t, "0", r.Native.UnmatchedOriginalUse)
	// Returning only one invocation cannot erase the other active execution.
	require.Contains(t, r.Reasons, "known_execution_active")
	require.Contains(t, r.Reasons, "continuation_unproven")
}

func TestMigrationRetentionLocalBillingIsOnlyObservation(t *testing.T) {
	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)
	require.NoError(
		t,
		f.db.Create(
			&model.NativeTask{
				ID:                  "completed-old",
				ChannelID:           f.body.Command.Seal.From.ChannelID,
				Status:              "completed",
				BillingSettled:      false,
				NativeInput:         "private-payload-that-must-not-leak",
				PrepaymentQuoteJSON: "private-payload-that-must-not-leak",
			},
		).Error,
	)
	r := retentionRead(t, f)
	require.Equal(t, "ready", r.State)
	require.Equal(t, "1", r.Native.LocalBillingUnsettled)
	require.Equal(t, "unproven", r.Retention)
	require.NotContains(t, r.Reasons, "native_task_not_terminal")
	require.NotContains(t, r.Reasons, "physical_billing_blocker")
}

func TestMigrationRetentionMissingCorruptAndLimit(t *testing.T) {
	for _, mode := range []string{"missing-table", "missing-protocol", "future-protocol", "partial-schema", "invalid-use", "limit", "missing-prepared", "wrong-head"} {
		t.Run(mode, func(t *testing.T) {
			f := newProbeHTTPFixture(t)
			retentionCommitted(t, f)

			switch mode {
			case "missing-table":
				require.NoError(t, f.db.Migrator().DropTable(&model.ChannelAdmissionExecution{}))
			case "missing-protocol":
				require.NoError(t, f.db.Delete(&model.ChannelAdmissionExecutionProtocol{}, 1).Error)
			case "future-protocol":
				require.NoError(
					t,
					f.db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id=1").
						UpdateColumn("version", 99).
						Error,
				)
			case "partial-schema":
				require.NoError(
					t,
					f.db.Exec(
						"ALTER TABLE channel_admission_executions DROP COLUMN continuation_state",
					).Error,
				)
			case "invalid-use":
				u := retentionUse(t, f, 4)
				u.State = "future-unknown"
				require.NoError(t, f.db.Create(&u).Error)
			case "limit":
				rows := make([]model.ChannelAdmissionUse, model.MigrationRetentionMaxRows+1)
				for i := range rows {
					rows[i] = retentionUse(t, f, i+100)
				}

				require.NoError(t, f.db.CreateInBatches(rows, 100).Error)
			case "missing-prepared":
				require.NoError(
					t,
					f.db.Where("operation_id=?", "http-probe-op").
						Delete(&model.ChannelMigrationPrepared{}).
						Error,
				)
			case "wrong-head":
				require.NoError(
					t,
					f.db.Model(&model.ChannelAdmissionFamily{}).
						Where("1=1").
						UpdateColumn("operation_id", "other-operation").
						Error,
				)
			}

			r := retentionRead(t, f)
			switch mode {
			case "missing-table", "missing-protocol":
				require.Equal(t, "ready", r.State)
				require.Nil(t, retentionSource(t, r, "relay").Counts)
			case "future-protocol":
				require.Equal(t, "invalid_protocol", retentionSource(t, r, "relay").State)
			case "invalid-use":
				require.Equal(t, "1", r.Uses.InvalidRecorded)
				require.Equal(t, "unproven", r.Retention)
			default:
				require.NotEqual(t, "ready", r.State)
				require.Nil(t, r.Uses)
				require.Nil(t, r.Native)
				require.Empty(t, r.Executions)
			}

			if mode == "limit" {
				require.Equal(t, "retention_source_limit_exceeded", r.Reason)
			}
		})
	}
}

func TestMigrationRetentionHTTPAuthAndNoDML(t *testing.T) {
	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)

	for _, spec := range []struct {
		path, key, body string
		code            int
	}{
		{retentionPath, "", "", 401}, {retentionPath + "?key=x", config.AdminKey, "", 401}, {retentionPath + "?channelId=11", config.AdminKey, "", 401}, {retentionPath, config.AdminKey, "{}", 400},
	} {
		req, e := http.NewRequestWithContext(t.Context(),
			http.MethodGet,
			f.server.URL+spec.path,
			strings.NewReader(spec.body),
		)
		require.NoError(t, e)

		if spec.key != "" {
			req.Header.Set("Authorization", "Bearer "+spec.key)
		}

		res, e := http.DefaultClient.Do(req)
		require.NoError(t, e)

		_, copyErr := io.Copy(io.Discard, res.Body)
		require.NoError(t, copyErr)
		res.Body.Close()
		require.Equal(t, spec.code, res.StatusCode)
		require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	}

	var tables []string
	require.NoError(
		t,
		f.db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").
			Scan(&tables).
			Error,
	)

	for i, table := range tables {
		for _, kind := range []string{"INSERT", "UPDATE", "DELETE"} {
			require.NoError(
				t,
				f.db.Exec(
					fmt.Sprintf(
						"CREATE TRIGGER retention_dml_%d_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'read must not write'); END",
						i,
						kind,
						kind,
						table,
					),
				).Error,
			)
		}
	}

	before := retentionRead(t, f)
	require.Equal(t, "ready", before.State)
	require.Error(
		t,
		f.db.Exec("UPDATE channel_admission_controls SET revision=revision").Error,
	) // triggers are effective
	after := retentionRead(t, f)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, before.Receipt, after.Receipt)
	_, raw := f.call(
		t,
		"GET",
		"/api/channel-migrations/operations/absent-op/retention",
		nil,
		config.AdminKey,
	)
	require.Contains(t, string(raw), `"state":"absent"`)
}

func TestMigrationRetentionAllInstalledSources(t *testing.T) {
	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)
	require.NoError(
		t,
		f.db.AutoMigrate(
			&model.ChannelAdmissionDispatchHandoff{},
			&model.ChannelAdmissionRecoveryHandoff{},
			&model.ChannelAdmissionImageRecoveryHandoff{},
			&model.ChannelAdmissionImagePollHandoff{},
			&model.ChannelAdmissionImageProducer{},
			&model.ChannelAdmissionMeasuredProducer{},
			&model.ChannelAdmissionMeasuredClaim{},
		),
	)

	for _, id := range []int{3, 4, 6, 7, 8, 9} {
		require.NoError(
			t,
			f.db.Create(&model.ChannelAdmissionExecutionProtocol{ID: id, Version: 1}).Error,
		)
	}

	u := retentionUse(t, f, 1)
	require.NoError(t, f.db.Create(&u).Error)
	image := model.ChannelAdmissionImageRecoveryHandoff{
		ID:                strings.Repeat("a", 64),
		Version:           1,
		UseID:             u.ID,
		UseHash:           retentionUseHash(u),
		Epoch:             u.Epoch,
		StorageGeneration: "probe-http-storage",
		State:             "accepted",
		ExecutionState:    "active",
		ContinuationState: "unproven",
		OfferedAtMs:       u.AdmittedAtMs,
		ClaimUntilMicros:  1,
	}
	require.NoError(t, f.db.Create(&image).Error)
	require.NoError(
		t,
		f.db.Create(
			&model.ChannelAdmissionImagePollHandoff{ChannelAdmissionImageRecoveryHandoff: image},
		).Error,
	)

	n := model.ChannelAdmissionTaskExecution{
		ID:                strings.Repeat("b", 64),
		UseID:             u.ID,
		UseHash:           retentionUseHash(u),
		Version:           2,
		Epoch:             u.Epoch,
		StorageGeneration: "probe-http-storage",
		State:             "active",
		ContinuationState: "unproven",
		AuthorizationHash: strings.Repeat("d", 64),
		StartedAtMs:       u.AdmittedAtMs,
	}
	require.NoError(t, f.db.Create(&n).Error)
	require.NoError(
		t,
		f.db.Create(
			&model.ChannelAdmissionDispatchHandoff{
				ID:            n.ID,
				Version:       1,
				ExecutionHash: n.AuthorizationHash,
				State:         "offered",
				OfferedAtMs:   u.AdmittedAtMs,
			},
		).Error,
	)
	require.NoError(
		t,
		f.db.Create(
			&model.ChannelAdmissionRecoveryHandoff{
				ID:            n.ID,
				Version:       1,
				ExecutionHash: n.AuthorizationHash,
				State:         "accepted",
				OfferedAtMs:   u.AdmittedAtMs,
			},
		).Error,
	)
	require.NoError(
		t,
		f.db.Create(
			&model.ChannelAdmissionImageProducer{
				ID:                strings.Repeat("c", 64),
				Version:           1,
				UseID:             u.ID,
				UseHash:           retentionUseHash(u),
				Epoch:             u.Epoch,
				StorageGeneration: "probe-http-storage",
				State:             "returned",
				ContinuationState: "unproven",
				StartedAtMs:       u.AdmittedAtMs,
				ReturnedAtMs:      u.AdmittedAtMs + 1,
			},
		).Error,
	)
	mp := model.ChannelAdmissionMeasuredProducer{
		ID:                strings.Repeat("e", 64),
		UseID:             u.ID,
		UseHash:           retentionUseHash(u),
		StorageGeneration: "probe-http-storage",
		State:             "returned",
		StartedAtMs:       u.AdmittedAtMs,
		ReturnedAtMs:      u.AdmittedAtMs + 1,
	}
	require.NoError(t, f.db.Create(&mp).Error)
	require.NoError(
		t,
		f.db.Create(
			&model.ChannelAdmissionMeasuredClaim{
				ID:                strings.Repeat("f", 64),
				SourceID:          mp.ID,
				Epoch:             u.Epoch,
				StorageGeneration: "probe-http-storage",
				State:             "accepted",
				OfferedAtMs:       u.AdmittedAtMs,
				UntilMicros:       1,
			},
		).Error,
	)
	r := retentionRead(t, f)
	require.Equal(t, "ready", r.State)
	require.Len(t, r.Executions, 9)

	for _, s := range r.Executions {
		require.Equal(t, "observed", s.State, s.Kind)
		require.Equal(t, "0", s.Counts.InvalidBindingOrState, s.Kind)
	}

	for _, kind := range []string{"native_dispatch", "native_recovery", "image_recovery", "image_poll"} {
		c := retentionSource(t, r, kind).Counts
		require.Equal(t, "1", c.UnfinishedHandoff, kind)
		require.Equal(t, "1", c.Active, kind)
	}

	require.Equal(t, "1", retentionSource(t, r, "measured_claim").Counts.UnfinishedHandoff)
	// A later returned row never closes a different offered/accepted worker.
	require.Equal(t, "required", r.Retention)
	require.Contains(t, r.Reasons, "known_handoff_unfinished")
}

// Pause after the first use SELECT while another actual connection commits
// changes to both uses and executions. A reader must not mix these generations.
func retentionConsistentSnapshot(t *testing.T, f *probeHTTPFixture) {
	t.Helper()
	retentionCommitted(t, f)
	u := retentionUse(t, f, 1)
	require.NoError(t, f.db.Create(&u).Error)
	ex := model.ChannelAdmissionExecution{
		ID:                strings.Repeat("a", 64),
		UseID:             u.ID,
		UseHash:           retentionUseHash(u),
		Version:           1,
		Epoch:             u.Epoch,
		StorageGeneration: "probe-http-storage",
		State:             "active",
		ContinuationState: "unproven",
		StartedAtMs:       u.AdmittedAtMs,
	}
	require.NoError(t, f.db.Create(&ex).Error)

	reached, release := make(chan struct{}), make(chan struct{})

	var once atomic.Bool

	require.NoError(
		t,
		f.db.Callback().
			Query().
			After("gorm:query").
			Register("retention_snapshot_pause", func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_admission_uses" &&
					once.CompareAndSwap(false, true) {
					close(reached)
					<-release
				}
			}),
	)
	defer func() {
		if err := f.db.Callback().Query().Remove("retention_snapshot_pause"); err != nil {
			t.Error(err)
		}
	}()

	done := make(chan model.MigrationRetentionResult, 1)
	go func() { done <- retentionRead(t, f) }()

	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not reach use snapshot")
	}

	err := f.db.Transaction(func(tx *gorm.DB) error {
		if e := tx.Model(&u).
			Updates(map[string]any{"state": "closed", "closed_at_ms": u.AdmittedAtMs + 1, "closure_hash": strings.Repeat("e", 64)}).
			Error; e != nil {
			return e
		}

		return tx.Model(&ex).
			Updates(map[string]any{"state": "returned", "returned_at_ms": ex.StartedAtMs + 1}).
			Error
	})

	close(release)
	require.NoError(t, err)

	r := <-done
	require.Equal(t, "ready", r.State)
	require.Equal(t, "1", r.Uses.ExactIdentityOpen)
	require.Equal(t, "1", retentionSource(t, r, "relay").Counts.Active)
	next := retentionRead(t, f)
	require.Equal(t, "ready", next.State)
	require.Equal(t, "1", next.Uses.ClosedRecorded)
	require.Equal(t, "0", retentionSource(t, next, "relay").Counts.Active)
	require.Equal(t, "1", retentionSource(t, next, "relay").Counts.Returned)
	require.Equal(t, "unproven", next.Retention)
	require.Contains(t, next.Reasons, "existing_barrier_or_use_management_guard")
}

func TestMigrationRetentionSQLiteConsistentSnapshot(t *testing.T) {
	f := newProbeHTTPFixture(t)
	require.NoError(t, f.db.Exec("PRAGMA journal_mode=WAL").Error)
	retentionConsistentSnapshot(t, f)
}

func TestMigrationRetentionAbsenceAndCancellation(t *testing.T) {
	for _, mode := range []string{"owned-resource", "paired-extension-missing", "partial-optional-schema", "future-optional-protocol", "cancelled-context"} {
		t.Run(mode, func(t *testing.T) {
			f := newProbeHTTPFixture(t)
			if mode == "owned-resource" {
				r := retentionRead(t, f)
				require.Equal(t, "conflict", r.State)
				require.Nil(t, r.Uses)
				return
			}

			switch mode {
			case "paired-extension-missing":
				require.NoError(t, f.db.Migrator().DropTable(&model.ChannelMigrationProbeRun{}))
			case "partial-optional-schema":
				require.NoError(
					t,
					f.db.Exec(
						"ALTER TABLE channel_migration_probe_runs DROP COLUMN evidence_task_hash",
					).Error,
				)
			case "future-optional-protocol":
				require.NoError(
					t,
					f.db.Model(&model.ChannelMigrationProbeRuntimeProtocol{}).
						Where("id=1").
						UpdateColumn("version", 99).
						Error,
				)
			case "cancelled-context":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				r := f.store.ReadMigrationRetention(ctx, "absent-op")
				require.NotEqual(t, "ready", r.State)
				require.NotEqual(t, "absent", r.State)
				require.Nil(t, r.Uses)

				return
			}

			_, raw := f.call(
				t,
				"GET",
				"/api/channel-migrations/operations/absent-op/retention",
				nil,
				config.AdminKey,
			)

			var r struct {
				Data model.MigrationRetentionResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(raw, &r))
			require.NotEqual(t, "absent", r.Data.State)
			require.Nil(t, r.Data.Uses)
		})
	}
}
