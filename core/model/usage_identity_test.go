//nolint:testpackage // The reviewed-SQL check needs the internal Postgres harness.
package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The app pins the same literal in src/modules/gateway/key-name.ts; both
// repositories must change it together.
func TestPlaygroundCredentialNameIsPinned(t *testing.T) {
	require.Equal(t, "__website_playground__", PlaygroundCredentialName)
	require.LessOrEqual(t, len(PlaygroundCredentialName), 32, "tokens.name is varchar(32)")
}

func TestUsageSource(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tokenName string
		internal  bool
		header    string
		want      string
	}{
		{"hidden credential in a customer group, no header", PlaygroundCredentialName, false, "", RequestSourcePlayground},
		// Old: playground. The header is unsigned; any key could claim it.
		{"customer key with a playground header", "playground", false, RequestSourcePlayground, RequestSourceAPI},
		{"internal group with admin_demo", "ops", true, RequestSourceAdminDemo, RequestSourceAdminDemo},
		{"hidden credential with an admin_demo header", PlaygroundCredentialName, false, RequestSourceAdminDemo, RequestSourcePlayground},
		// TokenAuth identified the key but rejected it: no group in context.
		{"identified but rejected hidden credential", PlaygroundCredentialName, false, "", RequestSourcePlayground},
		{"no token", "", false, RequestSourcePlayground, RequestSourceAPI},
		{"customer group cannot claim admin_demo", "prod", false, RequestSourceAdminDemo, RequestSourceAPI},
		{"internal group without the header", "ops", true, "", RequestSourceAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, UsageSource(tc.tokenName, tc.internal, tc.header))
		})
	}
}

func TestUsageIdentityCompleteAndTime(t *testing.T) {
	at := time.Date(2026, 10, 12, 3, 4, 5, 123_999_999, time.FixedZone("CST", 8*3600))
	identity := NewUsageIdentity(7, "vendor/model", "text-to-image", "", at)

	require.Equal(t, "vendor/model/text-to-image", identity.ModelID)
	require.Equal(t, RequestSourceAPI, identity.Source, "an empty source is api")
	require.Equal(t, time.Date(2026, 10, 11, 19, 4, 5, 123_000_000, time.UTC), identity.UsageAt,
		"UTC, truncated (never rounded) to the millisecond")
	require.Equal(t, at.UnixMilli(), identity.UsageAtMs())
	require.True(t, identity.Complete())

	for name, broken := range map[string]UsageIdentity{
		"no key":         {ModelID: "m", Source: RequestSourceAPI, UsageAt: at},
		"no model":       {APIKeyID: 7, Source: RequestSourceAPI, UsageAt: at},
		"long model":     {APIKeyID: 7, ModelID: strings.Repeat("m", MaxUsageModelIDLength+1), Source: RequestSourceAPI, UsageAt: at},
		"control char":   {APIKeyID: 7, ModelID: "m\n", Source: RequestSourceAPI, UsageAt: at},
		"unknown source": {APIKeyID: 7, ModelID: "m", Source: "web", UsageAt: at},
		"no time":        {APIKeyID: 7, ModelID: "m", Source: RequestSourceAPI},
	} {
		require.False(t, broken.Complete(), name)
	}

	require.True(t, NewUsageIdentity(1, "m", "", RequestSourceAPI, time.Time{}).UsageAt.IsZero())

	var info AsyncUsageInfo
	_, ok := info.UsageIdentity()
	require.False(t, ok, "an info without usage columns was created before P2")

	info.TokenID = 7
	info.SetUsageIdentity(identity)
	stored, ok := info.UsageIdentity()
	require.True(t, ok)
	require.Equal(t, identity, stored)
}

func usageIdentityDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := OpenSQLite(filepath.Join(t.TempDir(), "usage-identity.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}, &AsyncUsageInfo{}, &ImageTask{}, &NativeTask{}, &RequestDetail{}))

	old := LogDB
	LogDB = db

	t.Cleanup(func() {
		LogDB = old

		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return db
}

func storedLog(t *testing.T, db *gorm.DB, where string, args ...any) *Log {
	t.Helper()

	var rows []Log
	require.NoError(t, db.Where(where, args...).Order("id").Find(&rows).Error)
	require.Len(t, rows, 1)

	return &rows[0]
}

// G2: for every call path, the identity built from the path's inputs (what
// its money payloads carry) equals the identity of the row it writes.
func TestUsageIdentityIsTheRowIdentity(t *testing.T) {
	requestAt := time.Date(2026, 10, 12, 1, 2, 3, 456_789_123, time.UTC)
	wantAt := time.Date(2026, 10, 12, 1, 2, 3, 456_000_000, time.UTC)

	t.Run("relay with public fields", func(t *testing.T) {
		db := usageIdentityDB(t)
		fields := OperationalFields{
			RequestSource:      RequestSourcePlayground,
			PublicModel:        "bytedance/seedance-1-0-pro",
			ResolvedCapability: "image-to-video",
			BillingOperationID: "op-relay-public",
		}
		require.NoError(t, RecordConsumeLog("relay-public", time.Time{}, requestAt, time.Time{}, time.Time{},
			"g", 200, 3, "seedance-upstream", "", 7, PlaygroundCredentialName, "/v1/videos", "", int(mode.Videos), "",
			0, nil, Usage{}, UsageContext{}, Price{}, Amount{}, "", nil, "", "", "", "", AsyncUsageStatusPending, fields))

		model, capability := CallLogModel("seedance-upstream", "", fields)
		want := NewUsageIdentity(7, model, capability, fields.RequestSource, requestAt)
		row := storedLog(t, db, "request_id = ?", "relay-public")

		require.Equal(t, want, row.UsageIdentity())
		require.Equal(t, "bytedance/seedance-1-0-pro/image-to-video", want.ModelID)
		require.Equal(t, wantAt, want.UsageAt)
		require.Equal(t, "op-relay-public", string(row.BillingOperationID))
	})

	t.Run("relay with only a video capability", func(t *testing.T) {
		db := usageIdentityDB(t)
		require.NoError(t, RecordConsumeLog("relay-video", time.Time{}, requestAt, time.Time{}, time.Time{},
			"g", 200, 3, "veo-3", "text-to-video", 7, "prod", "/v1/videos", "", int(mode.Videos), "",
			0, nil, Usage{}, UsageContext{}, Price{}, Amount{}, "", nil, "", "", "", "", AsyncUsageStatusPending,
			OperationalFields{BillingOperationID: "op-video"}))

		want := NewUsageIdentity(7, "veo-3", "text-to-video", "", requestAt)
		require.Equal(t, want, storedLog(t, db, "request_id = ?", "relay-video").UsageIdentity())
		require.Equal(t, "veo-3/text-to-video", want.ModelID)
		require.Equal(t, RequestSourceAPI, want.Source)
	})

	t.Run("measured final row", func(t *testing.T) {
		db := usageIdentityDB(t)
		fields := OperationalFields{PublicModel: "openai/gpt-image-1", ResolvedCapability: "text-to-image", BillingOperationID: "op-measured"}
		logModel, capability := CallLogModel("gpt-image-1", "", fields)
		zero := int64(0)
		info := &AsyncUsageInfo{
			RequestID: "measured", BillingOperationID: fields.BillingOperationID, RequestAt: requestAt,
			GroupID: "g", TokenID: 7, Model: "gpt-image-1",
			Amount: Amount{ImageBillingResult: &ImageBillingResult{State: "complete", AmountMicros: &zero}},
		}
		info.SetUsageIdentity(NewUsageIdentity(7, logModel, capability, fields.RequestSource, requestAt))
		entry := &Log{
			RequestID: "measured", RequestAt: requestAt, GroupID: "g", TokenID: 7, Model: logModel,
			Capability: capability, BillingOperationID: EmptyNullString(fields.BillingOperationID),
		}
		require.NoError(t, CreateMeasuredImageSettlement(entry, info))

		stored, ok := info.UsageIdentity()
		require.True(t, ok)
		require.Equal(t, stored, storedLog(t, db, "request_id = ?", "measured").UsageIdentity())
	})

	t.Run("image task reservation row", func(t *testing.T) {
		db := usageIdentityDB(t)
		pricingAt := requestAt.Add(2 * time.Second) // handler time, later than the request start
		fields := OperationalFields{RequestSource: RequestSourcePlayground, PublicModel: "fal/flux", ResolvedCapability: "text-to-image"}
		info := &AsyncUsageInfo{
			RequestID: "image-task", RequestAt: pricingAt, GroupID: "g", TokenID: 7, Model: "flux-entry",
			Capability: "text-to-image", Mode: int(mode.ImagesGenerations), BillingOperationID: "op-image",
		}
		logModel, capability := CallLogModel(info.Model, info.Capability, fields)
		info.SetUsageIdentity(NewUsageIdentity(7, logModel, capability, fields.RequestSource, requestAt))

		_, created, err := ReserveImageTask(&ImageTask{
			ID: "image-task", GroupID: "g", TokenID: 7, Model: "flux-entry", Fingerprint: "f",
			BillingOperationID: "op-image",
		}, info, fields)
		require.NoError(t, err)
		require.True(t, created)

		row := storedLog(t, db, "request_id = ?", "image-task")
		stored, ok := info.UsageIdentity()
		require.True(t, ok)
		require.Equal(t, stored, row.UsageIdentity())
		require.Equal(t, wantAt, row.RequestAt.UTC(), "the row's time is the request start")
		require.Equal(t, "op-image", string(row.BillingOperationID))

		var saved AsyncUsageInfo
		require.NoError(t, db.First(&saved, info.ID).Error)
		require.Equal(t, pricingAt, saved.RequestAt.UTC(), "request_at stays the pricing instant (C8)")
	})

	for _, id := range []string{"vendor/model", "vendor/model/text-to-image"} {
		t.Run("native "+id, func(t *testing.T) {
			db := usageIdentityDB(t)
			task, _, err := ReserveNativeTask(db, NativeTask{
				ID: "native", GroupID: "g", TokenID: 7, Model: id, Fingerprint: "f",
				OutputSchema: `{"type":"object"}`, BillingOperationID: "native:native",
			})
			require.NoError(t, err)

			info := NativeTaskLog{RequestAt: requestAt, RequestSource: RequestSourcePlayground, Mode: int(mode.NativeTasks)}
			require.NoError(t, RecordNativeTaskLog(db, task, info))

			want := NativeUsageIdentity(task, info)
			require.Equal(t, id, want.ModelID, "the native model id is the frozen contract id")
			require.Equal(t, want, storedLog(t, db, "request_id = ?", "native").UsageIdentity())
		})
	}

	t.Run("recovery-inserted native row", func(t *testing.T) {
		db := usageIdentityDB(t)
		_, _, err := ReserveNativeTask(db, NativeTask{
			ID: "recovered", GroupID: "g", TokenID: 7, Model: "vendor/model/text-to-image", Fingerprint: "f",
			OutputSchema: `{"type":"object"}`, BillingOperationID: "native:recovered",
		})
		require.NoError(t, err)
		require.NoError(t, TransitionNativeSubmission(db, "recovered", "g", 7, "reserved", "submitting", ""))

		receipt := `{"id":"native:recovered","status":"pending","usageAtMs":` +
			strconv.FormatInt(wantAt.UnixMilli(), 10) + `,"usageSource":"playground"}`
		require.NoError(t, SaveNativeBillingTerminal(db, "recovered", "g", 7, receipt))

		want := UsageIdentity{APIKeyID: 7, ModelID: "vendor/model/text-to-image", Source: RequestSourcePlayground, UsageAt: wantAt}
		require.Equal(t, want, storedLog(t, db, "request_id = ?", "recovered").UsageIdentity())
	})
}

// sqlStatements splits a reviewed SQL file into statements: comments are
// dropped and semicolons inside $$ bodies are kept.
func sqlStatements(script string) []string {
	var (
		statements []string
		current    strings.Builder
		dollar     bool
	)

	for _, line := range strings.Split(script, "\n") {
		if !dollar {
			if index := strings.Index(line, "--"); index >= 0 {
				line = line[:index]
			}
		}

		for i := 0; i < len(line); i++ {
			if strings.HasPrefix(line[i:], "$$") {
				dollar = !dollar
				current.WriteString("$$")
				i++

				continue
			}

			if line[i] == ';' && !dollar {
				if statement := strings.TrimSpace(current.String()); statement != "" {
					statements = append(statements, statement)
				}

				current.Reset()

				continue
			}

			current.WriteByte(line[i])
		}

		current.WriteByte('\n')
	}

	if statement := strings.TrimSpace(current.String()); statement != "" {
		statements = append(statements, statement)
	}

	return statements
}

func reviewedUsageSQL(t *testing.T, dialect string) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "migrations", "usage-attribution-v1", dialect+".sql"))
	require.NoError(t, err)

	return sqlStatements(string(raw))
}

// applyReviewedUsageSQL runs the file on one connection, statement by
// statement, the way psql -f does.
func applyReviewedUsageSQL(t *testing.T, db *gorm.DB, dialect string) error {
	t.Helper()

	sqlDB, err := db.DB()
	require.NoError(t, err)

	conn, err := sqlDB.Conn(context.Background())
	require.NoError(t, err)

	defer conn.Close()

	for _, statement := range reviewedUsageSQL(t, dialect) {
		if _, err := conn.ExecContext(context.Background(), statement); err != nil {
			if dialect == "postgres" {
				_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			}

			return errors.Join(errors.New(statement), err)
		}
	}

	return nil
}

// stripUsageColumns turns a migrated LogDB into the shape of an unmigrated P1
// database.
func stripUsageColumns(t *testing.T, db *gorm.DB) {
	t.Helper()

	for _, statement := range []string{
		"DROP INDEX IF EXISTS idx_logs_group_billing_op",
		"DROP INDEX IF EXISTS idx_logs_group_reqat",
		"DROP INDEX IF EXISTS idx_logs_group_token_reqat",
		"ALTER TABLE logs DROP COLUMN billing_operation_id",
		"ALTER TABLE async_usage_infos DROP COLUMN usage_model_id",
		"ALTER TABLE async_usage_infos DROP COLUMN usage_source",
		"ALTER TABLE async_usage_infos DROP COLUMN usage_at_ms",
	} {
		require.NoError(t, db.Exec(statement).Error, statement)
	}

	require.False(t, UsageAttributionStorageReady(db))
}

type ddlCapture struct {
	logger.Interface

	statements []string
}

func (c *ddlCapture) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	statement, _ := fc()
	c.statements = append(c.statements, statement)
}

var schemaChange = regexp.MustCompile(`(?i)\b(ALTER|CREATE TABLE|DROP)\b`)

func assertUsageColumnsMatchReviewedSQL(t *testing.T, db *gorm.DB, dialect string) {
	t.Helper()

	stripUsageColumns(t, db)
	require.NoError(t, applyReviewedUsageSQL(t, db, dialect))
	require.True(t, UsageAttributionStorageReady(db))

	// The tags must equal the reviewed SQL, or AutoMigrate would alter the
	// columns of a large production table without lock_timeout.
	capture := &ddlCapture{Interface: logger.Discard}
	require.NoError(t, db.Session(&gorm.Session{Logger: capture}).AutoMigrate(&Log{}, &AsyncUsageInfo{}))

	for _, statement := range capture.statements {
		require.NotRegexp(t, schemaChange, statement, "AutoMigrate changed the reviewed schema")
	}

	// 'native:' + a 128-character request id: the spec's varchar(128) fails here.
	op := "native:" + strings.Repeat("a", 128)
	require.Len(t, op, 135)
	require.NoError(t, db.Create(&Log{RequestID: "long-op", GroupID: "g", BillingOperationID: EmptyNullString(op)}).Error)

	// A rejection row without an op stores NULL, never ''.
	require.NoError(t, db.Create(&Log{RequestID: "rejected", GroupID: "g", Code: 402}).Error)

	var nulls int64
	require.NoError(t, db.Model(&Log{}).Where("request_id = ? AND billing_operation_id IS NULL", "rejected").Count(&nulls).Error)
	require.EqualValues(t, 1, nulls)

	info := &AsyncUsageInfo{RequestID: "outbox", GroupID: "g", TokenID: 7}
	info.SetUsageIdentity(NewUsageIdentity(7, strings.Repeat("m", MaxUsageModelIDLength), "", RequestSourcePlayground, time.Now()))
	require.NoError(t, db.Create(info).Error)

	var stored AsyncUsageInfo
	require.NoError(t, db.First(&stored, info.ID).Error)
	identity, ok := stored.UsageIdentity()
	require.True(t, ok)
	require.Len(t, identity.ModelID, MaxUsageModelIDLength)

	// The file is idempotent where its dialect allows it.
	if dialect == "postgres" {
		require.NoError(t, applyReviewedUsageSQL(t, db, dialect))
	}
}

// The columns reach an established Postgres LogDB only through the reviewed
// SQL: AutoMigrate would ALTER the live logs table without lock_timeout, so a
// binary deployed before the SQL refuses to migrate (and /api/status stays
// 503), whatever DISABLE_AUTO_MIGRATE_DB says.
func TestLogDBMigrationRefusesAnEstablishedLogDBWithoutTheReviewedSQL(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		withTestPostgresStoreDB(t, func() {
			// A fresh database has no table to lock: AutoMigrate creates everything.
			require.NoError(t, checkUsageAttributionMigration(LogDB))
			require.NoError(t, LogDB.AutoMigrate(&Log{}, &AsyncUsageInfo{}))
			require.NoError(t, checkUsageAttributionMigration(LogDB))

			// An unmigrated P1 LogDB: startup's migration stops before any ALTER.
			stripUsageColumns(t, LogDB)
			require.ErrorIs(t, migrateLogDB(100), ErrUsageAttributionSQLMissing)
			require.False(t, UsageAttributionStorageReady(LogDB), "nothing added the columns")

			require.NoError(t, applyReviewedUsageSQL(t, LogDB, "postgres"))
			require.NoError(t, checkUsageAttributionMigration(LogDB))
		})
	})

	t.Run("sqlite development LogDB migrates as before", func(t *testing.T) {
		db := usageIdentityDB(t)
		stripUsageColumns(t, db)
		require.NoError(t, checkUsageAttributionMigration(db))
	})
}

func TestUsageAttributionColumnsMatchReviewedSQL(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db := usageIdentityDB(t)
		assertUsageColumnsMatchReviewedSQL(t, db, "sqlite")

		for _, name := range usageAttributionIndexNames {
			var count int64
			require.NoError(t, db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&count).Error)
			require.EqualValues(t, 1, count, name)
		}

		// A development SQLite LogDB also gets the indexes from startup.
		fresh := usageIdentityDB(t)
		oldSQLite := common.UsingSQLite
		common.UsingSQLite = true

		t.Cleanup(func() { common.UsingSQLite = oldSQLite })
		require.NoError(t, CreateLogIndexes(fresh))

		for _, name := range usageAttributionIndexNames {
			var count int64
			require.NoError(t, fresh.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&count).Error)
			require.EqualValues(t, 1, count, name)
		}
	})

	t.Run("postgres", func(t *testing.T) {
		withTestPostgresStoreDB(t, func() {
			require.NoError(t, LogDB.AutoMigrate(&Log{}, &AsyncUsageInfo{}))

			missing, err := MissingUsageIndexes(LogDB)
			require.NoError(t, err)
			require.Equal(t, usageAttributionIndexNames, missing, "startup never builds them on Postgres")

			assertUsageColumnsMatchReviewedSQL(t, LogDB, "postgres")

			missing, err = MissingUsageIndexes(LogDB)
			require.NoError(t, err)
			require.Empty(t, missing)

			// The guard refuses the application database.
			require.NoError(t, LogDB.Exec("CREATE TABLE wallet_ledger (id text)").Error)
			err = applyReviewedUsageSQL(t, LogDB, "postgres")
			require.ErrorContains(t, err, "This is the application database")
			require.NoError(t, LogDB.Exec("DROP TABLE wallet_ledger").Error)
		})
	})
}
