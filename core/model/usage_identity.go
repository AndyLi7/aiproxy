package model

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// PlaygroundCredentialName is the reserved name of the website Playground's
// hidden per-user credential (owner decision D4, 2026-10-12). Only the
// application creates gateway tokens (AdminAuth routes), it never lists or
// reveals this one, and it refuses the name for customer keys, so a call made
// with it was made by the website. The application pins the same literal
// (src/modules/gateway/key-name.ts PLAYGROUND_CREDENTIAL_NAME); a test in each
// repository keeps the two equal.
//
//nolint:gosec // Symbolic credential name or synthetic test identifier, not a secret.
const PlaygroundCredentialName = "__website_playground__"

// AgentCredentialName is reserved by the application. Only AdminAuth provisions
// these server-held keys. Retired keys keep an explicit numeric suffix so tasks
// can be read with their original identity after rotation.
const AgentCredentialName = "__website_agent__"

func IsAgentCredentialName(name string) bool {
	if name == AgentCredentialName {
		return true
	}

	if !strings.HasPrefix(name, "__website_agent_") || !strings.HasSuffix(name, "__") {
		return false
	}

	suffix := strings.TrimSuffix(strings.TrimPrefix(name, "__website_agent_"), "__")
	if len(suffix) == 0 || len(name) > MaxTokenNameLength {
		return false
	}

	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}

// MaxUsageModelIDLength bounds a usage model id (the canonical capability key),
// equal to the application's limit and async_usage_infos.usage_model_id.
const MaxUsageModelIDLength = 256

// UsageIdentity is the attribution every call row and every money movement of
// that call carry (key usage P2): the key, the canonical model id, the source
// and the request start. It is built in one place (NewUsageIdentity) from the
// same inputs the call row stores, so a row and its payloads cannot disagree.
type UsageIdentity struct {
	APIKeyID int       // tokens.id
	ModelID  string    // CK (model/capability) when a capability exists, else the logged model
	Source   string    // api | playground | admin_demo
	UsageAt  time.Time // request start, UTC, truncated to the millisecond
}

// CallLogModel is the (model, capability) every call row stores: the public
// identity when the request resolved one, else the relay's own model and
// capability. RecordConsumeLog, the measured image row, the image task
// reservation row and the async usage outbox all use it.
func CallLogModel(originModel, capability string, f OperationalFields) (string, string) {
	if f.PublicModel != "" || f.ResolvedCapability != "" {
		return PublicLogIdentity(originModel, f.PublicModel, f.ResolvedCapability)
	}

	return originModel, capability
}

// UsageSource is the only place a call's source is decided. The website
// Playground is recognised by its credential, never by a client header: a
// playground header from any other key is recorded as api. admin_demo stays
// limited to internal groups.
func UsageSource(tokenName string, groupInternal bool, header string) string {
	switch {
	case groupInternal && header == RequestSourceAdminDemo:
		return RequestSourceAdminDemo
	case !groupInternal && IsAgentCredentialName(tokenName):
		return RequestSourceAgent
	case !groupInternal && tokenName == PlaygroundCredentialName:
		return RequestSourcePlayground
	default:
		return RequestSourceAPI
	}
}

// UsageTime is the instant a usage identity carries: UTC, truncated to the
// millisecond (the wallet stores milliseconds). The zero time stays zero.
func UsageTime(requestAt time.Time) time.Time {
	if requestAt.IsZero() {
		return time.Time{}
	}

	return time.UnixMilli(requestAt.UnixMilli()).UTC()
}

// UsageModelID is the canonical model id of a call row's (model, capability).
func UsageModelID(logModel, logCapability string) string {
	if logCapability == "" {
		return logModel
	}

	return logModel + "/" + logCapability
}

func NewUsageIdentity(
	tokenID int,
	logModel string,
	logCapability string,
	source string,
	requestAt time.Time,
) UsageIdentity {
	if source == "" {
		source = RequestSourceAPI
	}

	return UsageIdentity{
		APIKeyID: tokenID,
		ModelID:  UsageModelID(logModel, logCapability),
		Source:   source,
		UsageAt:  UsageTime(requestAt),
	}
}

// Complete reports an identity the wallet accepts. Payloads carry the four
// fields only when it is complete; a partial identity is never sent.
func (u UsageIdentity) Complete() bool {
	return u.APIKeyID > 0 &&
		validUsageModelID(u.ModelID) &&
		validUsageSource(u.Source) &&
		!u.UsageAt.IsZero() &&
		u.UsageAt.UnixMilli() > 0
}

// UsageAtMs is UsageAt in Unix milliseconds, as payloads send it.
func (u UsageIdentity) UsageAtMs() int64 {
	return u.UsageAt.UnixMilli()
}

func validUsageModelID(id string) bool {
	if id == "" || len(id) > MaxUsageModelIDLength || strings.TrimSpace(id) != id {
		return false
	}

	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}

	return true
}

func validUsageSource(source string) bool {
	switch source {
	case RequestSourceAPI, RequestSourcePlayground, RequestSourceAdminDemo, RequestSourceAgent:
		return true
	default:
		return false
	}
}

// ValidUsageSource reports a known, non-empty source.
func ValidUsageSource(source string) bool {
	return validUsageSource(source)
}

// UsageIdentity is the identity of a stored call row.
func (l *Log) UsageIdentity() UsageIdentity {
	return NewUsageIdentity(l.TokenID, l.Model, l.Capability, l.RequestSource, l.RequestAt)
}

// SetUsageIdentity stores the call's identity on the outbox. Every info a P2
// binary creates carries it, so UsageIdentity returning false means exactly
// "created by an earlier binary". The key is the info's own token_id.
func (i *AsyncUsageInfo) SetUsageIdentity(u UsageIdentity) {
	i.UsageModelID = EmptyNullString(u.ModelID)
	i.UsageSource = EmptyNullString(u.Source)
	i.UsageAtMs = ZeroNullInt64(u.UsageAtMs())
}

// UsageIdentity returns the identity stored on the outbox, or false for an
// info created before key usage P2 (its usage columns are NULL). The time is
// usage_at_ms, never request_at: request_at stays the pricing instant (C8).
func (i *AsyncUsageInfo) UsageIdentity() (UsageIdentity, bool) {
	if i == nil || i.UsageModelID == "" || i.UsageSource == "" || i.UsageAtMs <= 0 {
		return UsageIdentity{}, false
	}

	return UsageIdentity{
		APIKeyID: i.TokenID,
		ModelID:  string(i.UsageModelID),
		Source:   string(i.UsageSource),
		UsageAt:  time.UnixMilli(int64(i.UsageAtMs)).UTC(),
	}, true
}

// NativeUsageIdentity is the identity of a native task's call: its key, its
// frozen contract id (the CK) and the request's source and time.
func NativeUsageIdentity(task *NativeTask, info NativeTaskLog) UsageIdentity {
	if task == nil {
		return UsageIdentity{}
	}

	logModel, capability := nativeLogIdentity(task.Model)

	return NewUsageIdentity(
		task.TokenID,
		logModel,
		capability,
		nativeLogSource(info.RequestSource),
		info.RequestAt,
	)
}

// usageAttributionColumns are the LogDB columns the reviewed SQL
// migrations/usage-attribution-v1 adds and a P2 binary writes.
var usageAttributionColumns = []struct {
	model  any
	column string
}{
	{&Log{}, "billing_operation_id"},
	{&AsyncUsageInfo{}, "usage_model_id"},
	{&AsyncUsageInfo{}, "usage_source"},
	{&AsyncUsageInfo{}, "usage_at_ms"},
}

// ErrUsageAttributionSQLMissing refuses LogDB AutoMigrate on an established
// Postgres or MySQL LogDB that lacks the reviewed columns.
var ErrUsageAttributionSQLMissing = errors.New(
	"LogDB lacks logs.billing_operation_id or async_usage_infos.usage_model_id/usage_source/usage_at_ms:" +
		" apply migrations/usage-attribution-v1 before this binary;" +
		" AutoMigrate would ALTER the live logs table without lock_timeout",
)

// checkUsageAttributionMigration runs before LogDB AutoMigrate. On an
// established Postgres or MySQL LogDB (the logs table exists) the usage
// attribution columns come only from the reviewed SQL, so a binary deployed
// before it refuses to migrate instead of altering a live table: with the
// LogDB on the main database startup fails, a split LogDB retries in the
// background, and /api/status answers 503 meanwhile, so the previous binary
// keeps serving. A fresh database (no table to lock) and a development SQLite
// LogDB migrate as before.
func checkUsageAttributionMigration(db *gorm.DB) error {
	if db == nil || db.Name() == "sqlite" {
		return nil
	}

	if !db.Migrator().HasTable(&Log{}) || UsageAttributionStorageReady(db) {
		return nil
	}

	return ErrUsageAttributionSQLMissing
}

// UsageAttributionStorageReady reports whether the LogDB has every column the
// reviewed SQL adds. Like NativeTaskStorageReady it runs an empty projection:
// it reads no data and migrates nothing. A binary ahead of the schema would
// fail every insert into logs and async_usage_infos, and those failures are
// only logged (video and measured image calls would never be charged), so
// /api/status reports unhealthy until this is true.
func UsageAttributionStorageReady(db *gorm.DB) bool {
	if db == nil {
		return false
	}

	quiet := db.Session(&gorm.Session{Logger: logger.Discard, NewDB: true})
	for _, column := range usageAttributionColumns {
		rows, err := quiet.Model(column.model).Select(column.column).Limit(0).Rows()
		if err != nil {
			return false
		}

		err = rows.Err()
		//nolint:sqlclosecheck // The LIMIT 0 schema probe must close before the next query on this transaction.
		_ = rows.Close()

		if err != nil {
			return false
		}
	}

	return true
}
