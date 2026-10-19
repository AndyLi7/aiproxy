//nolint:testpackage // White-box coverage needs the package-private probeHTTPFixture fixture or implementation.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func retiredHTTPFixture(t *testing.T) (*probeHTTPFixture, model.MigrationRetiredResourceRequest) {
	t.Helper()
	return retiredHTTPFixtureFrom(t, newProbeHTTPFixture(t))
}

func retiredHTTPFixtureFrom(
	t *testing.T,
	f *probeHTTPFixture,
) (*probeHTTPFixture, model.MigrationRetiredResourceRequest) {
	t.Helper()

	forward, ok := probeCommand(f.body.Command)
	require.True(t, ok)

	raw := mustJSON(t, forward)
	hash := sha256.Sum256(raw)
	proof, _ := f.store.LoadDeployment(t.Context())
	fact := model.VerifiedMigrationProbe{
		RequestHash:       hex.EncodeToString(hash[:]),
		EvidenceHash:      strings.Repeat("a", 64),
		StorageGeneration: proof.StorageGeneration,
		IssuedAtMs:        proof.IssuedAtMs,
		ValidUntilMs:      proof.ValidUntilMs,
	}
	ph, e := fact.Fingerprint()
	require.NoError(t, e)
	require.NoError(
		t,
		f.db.Create(
			&model.ChannelMigrationProbeRecord{EvidenceHash: fact.EvidenceHash, FactHash: ph},
		).Error,
	)
	f.store.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
		return fact, nil
	}
	_, e = f.store.PrepareMigration(t.Context(), forward)
	require.NoError(t, e)
	_, e = f.store.CutoverPrepared(t.Context(), forward)
	require.NoError(t, e)
	require.NoError(t, f.db.AutoMigrate(&model.ChannelMigrationRetiredTargetResource{}))
	inv := f.store.ReadMigrationInventory(t.Context(), f.store.MigrationFeatures)
	require.Equal(t, "ready", inv.State)
	snap, e := f.store.ReadMigrationSnapshot(t.Context(), forward.Seal.To, forward.Seal.From)
	require.NoError(t, e)

	q := model.MigrationRetiredResourceRequest{
		Version:            1,
		SourceSnapshotHash: inv.SourceSnapshotHash,
		Command: model.MigrationReadRequest{
			TargetResourceKind: model.MigrationReuseRetiredTarget,
			Seal: model.MigrationReadSealRequest{
				OperationID:      "reverse-http",
				Epoch:            snap.Epoch,
				ExpectedRevision: strconv.FormatInt(snap.Revision, 10),
				From:             forward.Seal.To,
				To:               forward.Seal.From,
				RevertOf:         forward.Seal.OperationID,
			},
			FromCredentialID: forward.ToCredentialID,
			ToCredentialID:   forward.FromCredentialID,
			ExpectedPinHash:  snap.PinHash,
			ProjectionHash:   snap.ProjectionHash,
			FamilyHash:       snap.FamilyHash,
		},
	}

	return f, q
}

func retiredHTTPCall(
	t *testing.T,
	f *probeHTTPFixture,
	q model.MigrationRetiredResourceRequest,
	suffix string,
	body any,
) model.MigrationRetiredResourceResult {
	t.Helper()
	code, raw := f.call(
		t,
		"POST",
		"/api/channel-migrations/operations/"+q.Command.Seal.OperationID+"/retired-target"+suffix,
		body,
		config.AdminKey,
	)
	require.Equal(t, 200, code, string(raw))

	var out struct {
		Data model.MigrationRetiredResourceResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "gateway-migration-retired-target-v1", out.Data.Source)

	for _, secret := range []string{"synthetic-probe-shadow-key", `"key":`, `"RequestJSON"`, `"shadow":`} {
		require.NotContains(t, string(raw), secret)
	}

	return out.Data
}

func retiredReadBody(q model.MigrationRetiredResourceRequest) any {
	return map[string]any{"version": 1, "request": q}
}

func prepareRetiredHTTP(
	t *testing.T,
	f *probeHTTPFixture,
	q model.MigrationRetiredResourceRequest,
) (model.MigrationProbeCutoverBinding, model.NativeTask) {
	t.Helper()
	created := retiredHTTPCall(t, f, q, "", q)
	require.Equal(t, "ready", created.State)

	b := f.body
	b.Command = q.Command
	b.ResourceRequestHash = created.Observation.RequestHash
	b.CheckpointHash = created.Observation.CheckpointHash
	path := "/api/channel-migrations/operations/" + q.Command.Seal.OperationID
	_, raw := f.call(t, "POST", path+"/probe", b, config.AdminKey)

	var obs struct {
		Data model.MigrationProbeResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &obs))
	require.NotNil(t, obs.Data.Observation, string(raw))
	require.Equal(t, "queued", obs.Data.Observation.TaskStatus, string(raw))
	_, _ = f.call(t, "POST", path+"/probe", b, config.AdminKey)
	require.Equal(t, 1, f.providerCalls)
	task, e := f.engine.Poll(
		t.Context(),
		obs.Data.Observation.TaskID,
		b.GroupID,
		b.TokenID,
		func(context.Context, *model.NativeTask) (nativetask.Poller, error) {
			return probeCompletedPoller{}, nil
		},
		func(_ context.Context, id string, index int, _ string) (ownedartifact.Receipt, error) {
			return ownedartifact.Receipt{
				Key: fmt.Sprintf(
					"native-results/%s/%d/%s.bin",
					id,
					index,
					strings.Repeat("a", 64),
				),
				SHA256: strings.Repeat("b", 64),
				Size:   23,
			}, nil
		},
	)
	require.NoError(t, e)

	f.billingReader = func(_ context.Context, r model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
		return model.MigrationProbeBillingEvidence{
			Binding:                       r,
			EvidenceID:                    "reverse-http-bill",
			ProvenanceHash:                strings.Repeat("d", 64),
			AccountMembershipEvidenceHash: strings.Repeat("e", 64),
			AccountMembershipEvidenceKind: "fal-workspace-key-membership-v1",
			Currency:                      "USD",
			CostMicros:                    600000,
			CustomerDueMicros:             900000,
			ChargedMicros:                 900000,
			SettledAtMs:                   time.Now().Add(-time.Second).UnixMilli(),
		}, nil
	}
	_, raw = f.call(
		t,
		"POST",
		path+"/prepare",
		map[string]any{"version": 1, "authorizationHash": obs.Data.Observation.AuthorizationHash},
		config.AdminKey,
	)

	var read struct {
		Data model.MigrationReadResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &read))
	require.Equal(t, "ready", read.Data.State, string(raw))
	require.NotNil(t, read.Data.Readback)
	r := read.Data.Readback.Receipt

	return model.MigrationProbeCutoverBinding{
		Version:           1,
		AuthorizationHash: obs.Data.Observation.AuthorizationHash,
		RequestHash:       r.RequestHash,
		ProbeFactHash:     r.ProbeFactHash,
		StorageGeneration: r.StorageGeneration,
	}, *task
}

func TestMigrationRetiredHTTPLostCreateAndCancel(t *testing.T) {
	f, q := retiredHTTPFixture(t)
	require.Equal(t, "absent", retiredHTTPCall(t, f, q, "/read", retiredReadBody(q)).State)
	created := retiredHTTPCall(t, f, q, "", q)
	require.Equal(t, "ready", created.State)
	// No create receipt/checkpoint is supplied to recovery.
	read := retiredHTTPCall(t, f, q, "/read", retiredReadBody(q))
	require.Equal(t, created, read)
	require.Equal(t, created, retiredHTTPCall(t, f, q, "", q))
	changed := q
	changed.Command.ProjectionHash = strings.Repeat("f", 64)
	require.Equal(
		t,
		"conflict",
		retiredHTTPCall(t, f, changed, "/read", retiredReadBody(changed)).State,
	)

	cancel := map[string]any{
		"version":             1,
		"resourceRequestHash": read.Observation.RequestHash,
		"checkpointHash":      read.Observation.CheckpointHash,
	}
	cancelled := retiredHTTPCall(t, f, q, "/cancel", cancel)
	require.Equal(t, "ready", cancelled.State)
	require.Equal(t, "cancelled", cancelled.Observation.Lifecycle.State)
	require.Equal(t, cancelled, retiredHTTPCall(t, f, q, "/read", retiredReadBody(q)))
	require.Equal(t, 0, f.providerCalls)

	var channels []model.Channel
	require.NoError(t, f.db.Find(&channels).Error)
	require.Len(t, channels, 2)

	for _, c := range channels {
		require.Equal(t, model.ChannelStatusEnabled, c.Status)
	}
}

func TestMigrationRetiredHTTPActualProbePrepareCutover(t *testing.T) {
	for _, mode := range []string{"success", "cancel", "authorization", "request", "fact", "storage", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f, q := retiredHTTPFixture(t)
			if mode == "unknown" {
				created := retiredHTTPCall(t, f, q, "", q)
				require.Equal(t, "ready", created.State)

				b := f.body
				b.Command = q.Command
				b.ResourceRequestHash = created.Observation.RequestHash
				b.CheckpointHash = created.Observation.CheckpointHash
				f.loseProviderReply = true

				path := "/api/channel-migrations/operations/" + q.Command.Seal.OperationID + "/probe"
				for range 2 {
					_, raw := f.call(t, "POST", path, b, config.AdminKey)
					require.Contains(t, string(raw), `"state":"unknown"`)
				}

				require.Equal(t, 1, f.providerCalls)

				return
			}

			b, task := prepareRetiredHTTP(t, f, q)

			path := "/api/channel-migrations/operations/" + q.Command.Seal.OperationID
			switch mode {
			case "authorization":
				b.AuthorizationHash = strings.Repeat("0", 64)
			case "request":
				b.RequestHash = strings.Repeat("0", 64)
			case "fact":
				b.ProbeFactHash = strings.Repeat("0", 64)
			case "storage":
				b.StorageGeneration = "other"
			case "cancel":
				read := retiredHTTPCall(t, f, q, "/read", retiredReadBody(q))
				r := retiredHTTPCall(
					t,
					f,
					q,
					"/cancel",
					map[string]any{
						"version":             1,
						"resourceRequestHash": read.Observation.RequestHash,
						"checkpointHash":      read.Observation.CheckpointHash,
					},
				)
				require.Equal(t, "ready", r.State)
			}

			_, raw := f.call(t, "POST", path+"/cutover", b, config.AdminKey)
			if mode == "success" {
				require.Contains(t, string(raw), `"state":"committed"`)
				_, replay := f.call(t, "POST", path+"/cutover", b, config.AdminKey)
				require.Equal(t, string(raw), string(replay))
			} else {
				require.NotContains(t, string(raw), `"state":"committed"`)
			}

			var after model.NativeTask
			require.NoError(t, f.db.First(&after, "id=?", task.ID).Error)
			require.Equal(t, task.PrepaymentQuoteJSON, after.PrepaymentQuoteJSON)
			require.Equal(t, task.BillingReceiptJSON, after.BillingReceiptJSON)
			_, bill := f.call(
				t,
				"POST",
				"/api/channel-migrations/probe-billing-context/read",
				model.MigrationProbeBillingContextRequest{
					BillingOperationID: task.BillingOperationID,
					TaskID:             task.ID,
					GroupID:            task.GroupID,
				},
				config.AdminKey,
			)
			require.Contains(t, string(bill), `"state":"ready"`)
			require.Contains(t, string(bill), `"targetResourceKind":"reuse_retired_target"`)
			require.Equal(t, 1, f.providerCalls)

			var channels []model.Channel
			require.NoError(t, f.db.Find(&channels).Error)
			require.Len(t, channels, 2)

			for _, c := range channels {
				require.Equal(t, model.ChannelStatusEnabled, c.Status)
			}
		})
	}
}

func TestMigrationRetiredHTTPStrictInput(t *testing.T) {
	f, q := retiredHTTPFixture(t)
	raw, e := json.Marshal(q)
	require.NoError(t, e)

	for _, suffix := range []string{"", "/read", "/cancel"} {
		var body any = q
		if suffix == "/read" {
			body = retiredReadBody(q)
		}

		if suffix == "/cancel" {
			body = map[string]any{
				"version":             1,
				"resourceRequestHash": strings.Repeat("a", 64),
				"checkpointHash":      strings.Repeat("b", 64),
			}
		}

		path := "/api/channel-migrations/operations/" + q.Command.Seal.OperationID + "/retired-target" + suffix
		for _, key := range []string{"", "bad"} {
			code, _ := f.call(t, "POST", path, body, key)
			require.Equal(t, 401, code)
		}

		code, _ := f.call(t, "POST", path+"?key=x", body, config.AdminKey)
		require.Equal(t, 401, code)

		for _, invalid := range []string{`{}`, `null`, `{"version":1,"version":1}`, `{"version":null}`, strings.Repeat("x", migrationReadBodyLimit+1)} {
			code, _ := f.call(t, "POST", path, invalid, config.AdminKey)
			require.Equal(t, 400, code, invalid[:min(40, len(invalid))])
		}
	}

	path := "/api/channel-migrations/operations/" + q.Command.Seal.OperationID + "/retired-target"
	for _, input := range []string{strings.Replace(string(raw), `"version":1`, `"version":1,"unexpected":true`, 1), strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(raw), `"revertOf":"http-probe-op"`, `"revertOf":""`, 1), strings.Replace(string(raw), `"targetResourceKind":"reuse_retired_target"`, `"targetResourceKind":""`, 1), strings.Replace(string(raw), `"expectedRevision":"`, `"expectedRevision":"0`, 1)} {
		code, _ := f.call(t, "POST", path, input, config.AdminKey)
		require.Equal(t, 400, code, input)
	}
	// A real reverse command disguised as forward cannot borrow the original shadow.
	created := retiredHTTPCall(t, f, q, "", q)
	require.Equal(t, "ready", created.State)

	b := f.body
	b.Command = q.Command
	b.ResourceRequestHash = created.Observation.RequestHash
	b.CheckpointHash = created.Observation.CheckpointHash
	b.Command.TargetResourceKind = ""
	b.Command.Seal.RevertOf = ""
	_, denied := f.call(
		t,
		"POST",
		"/api/channel-migrations/operations/"+q.Command.Seal.OperationID+"/probe",
		b,
		config.AdminKey,
	)
	require.NotContains(t, string(denied), `"taskStatus":"queued"`)
	require.Equal(t, 0, f.providerCalls)
}

func TestMigrationRetiredHTTPReadNoDMLAndNoIssuer(t *testing.T) {
	f, q := retiredHTTPFixture(t)
	created := retiredHTTPCall(t, f, q, "", q)
	require.Equal(t, "ready", created.State)

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
						"CREATE TRIGGER retired_no_dml_%d_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'read cannot write'); END",
						i,
						kind,
						kind,
						table,
					),
				).Error,
			)
		}
	}

	restore := model.ConfigureChannelAdmissionRuntime(model.ChannelExecutorIdentity{}, nil)
	defer restore()

	require.Error(t, f.db.Exec("UPDATE channel_admission_controls SET revision=revision").Error)
	require.Equal(t, created, retiredHTTPCall(t, f, q, "/read", retiredReadBody(q)))
	require.Equal(t, 0, f.providerCalls)
}

func TestMigrationRetiredHTTPReadIncompleteSources(t *testing.T) {
	for _, mode := range []string{"missing-table", "missing-column", "wrong-version", "missing-retirement", "wrong-checkpoint", "missing-original", "absent-with-probe", "absent-partial-probe"} {
		t.Run(mode, func(t *testing.T) {
			f, q := retiredHTTPFixture(t)
			if !strings.HasPrefix(mode, "absent-") {
				require.Equal(t, "ready", retiredHTTPCall(t, f, q, "", q).State)
			}

			switch mode {
			case "missing-table":
				require.NoError(
					t,
					f.db.Migrator().DropTable(&model.ChannelMigrationRetiredTargetResource{}),
				)
			case "missing-column":
				require.NoError(
					t,
					f.db.Exec(
						"ALTER TABLE channel_migration_retired_target_resources DROP COLUMN retirement_request_hash",
					).Error,
				)
				require.False(
					t,
					f.db.Migrator().
						HasColumn(&model.ChannelMigrationRetiredTargetResource{}, "retirement_request_hash"),
				)
			case "wrong-version":
				require.NoError(
					t,
					f.db.Model(&model.ChannelMigrationRetiredTargetResource{}).
						Where("operation_id=?", q.Command.Seal.OperationID).
						UpdateColumn("version", 99).
						Error,
				)
			case "missing-retirement":
				require.NoError(
					t,
					f.db.Delete(
						&model.ChannelAdmissionBarrier{},
						"operation_id=?",
						q.Command.Seal.RevertOf,
					).Error,
				)
			case "wrong-checkpoint":
				require.NoError(
					t,
					f.db.Model(&model.ChannelMigrationRetiredTargetResource{}).
						Where("operation_id=?", q.Command.Seal.OperationID).
						UpdateColumn("checkpoint_hash", strings.Repeat("0", 64)).
						Error,
				)
			case "missing-original":
				require.NoError(
					t,
					f.db.Delete(
						&model.ChannelMigrationPrepared{},
						"operation_id=?",
						q.Command.Seal.RevertOf,
					).Error,
				)
			case "absent-with-probe":
				require.NoError(
					t,
					f.db.Create(
						&model.ChannelMigrationProbeRun{OperationID: q.Command.Seal.OperationID},
					).Error,
				)
			case "absent-partial-probe":
				require.NoError(
					t,
					f.db.Exec(
						"ALTER TABLE channel_migration_probe_runs DROP COLUMN evidence_task_hash",
					).Error,
				)
			}

			r := retiredHTTPCall(t, f, q, "/read", retiredReadBody(q))
			require.NotEqual(t, "ready", r.State)
			require.NotEqual(t, "absent", r.State)
			require.Nil(t, r.Observation)
		})
	}
}

func TestMigrationRetiredHTTPRejectsWrongTargetOrKind(t *testing.T) {
	for _, mode := range []string{"forward", "missing-revert", "other-revert", "wrong-target", "unsafe-id", "wrong-snapshot"} {
		t.Run(mode, func(t *testing.T) {
			f, q := retiredHTTPFixture(t)
			switch mode {
			case "forward":
				q.Command.TargetResourceKind = ""
				q.Command.Seal.RevertOf = ""
			case "missing-revert":
				q.Command.Seal.RevertOf = ""
			case "other-revert":
				q.Command.Seal.RevertOf = "other-original"
			case "wrong-target":
				q.Command.Seal.To = q.Command.Seal.From
			case "unsafe-id":
				q.Command.Seal.To.ChannelID = 9007199254740992
			case "wrong-snapshot":
				q.SourceSnapshotHash = strings.Repeat("0", 64)
			}

			_, raw := f.call(
				t,
				"POST",
				"/api/channel-migrations/operations/"+q.Command.Seal.OperationID+"/retired-target",
				q,
				config.AdminKey,
			)
			require.NotContains(t, string(raw), `"state":"ready"`)

			var n int64
			require.NoError(
				t,
				f.db.Model(&model.ChannelMigrationRetiredTargetResource{}).Count(&n).Error,
			)
			require.Zero(t, n)
			require.Equal(t, 0, f.providerCalls)
		})
	}
}
