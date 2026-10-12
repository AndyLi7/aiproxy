package model

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

type AsyncUsageStatus int

const (
	AsyncUsageStatusNone AsyncUsageStatus = iota
	AsyncUsageStatusPending
	AsyncUsageStatusCompleted
	AsyncUsageStatusFailed
	// Immutable synchronous response lacks billable evidence; never poll upstream.
	AsyncUsageStatusMeasurementPending
)

const (
	AsyncUsageDefaultPollDelay = 3 * time.Second
	AsyncUsageMaxPollDelay     = 3 * time.Minute
)

var asyncUsageSchemaCache sync.Map

type AsyncUsageInfo struct {
	// Server-owned admission snapshot; never accepted from public JSON requests.
	InternalImageTask bool   `json:"-"`
	MeasuredImage     bool   `json:"measured_image,omitempty"`
	ID                int    `json:"id"                                       gorm:"primaryKey"`
	RequestID         string `json:"request_id"                               gorm:"type:varchar(128);index"`
	// Persist before settlement; empty on pre-upgrade rows to preserve prior debit keys.
	BillingOperationID          string           `json:"-" gorm:"type:varchar(128)"`
	RequestAt                   time.Time        `json:"request_at"`
	Mode                        int              `json:"mode"                                     gorm:"index"`
	Model                       string           `json:"model"                                    gorm:"size:128"`
	Capability                  string           `json:"capability,omitempty"                     gorm:"size:64;index"`
	ChannelID                   int              `json:"channel_id"                               gorm:"index"`
	BaseURL                     string           `json:"base_url,omitempty"                       gorm:"type:text"`
	GroupID                     string           `json:"group_id"                                 gorm:"size:64;index"`
	TokenID                     int              `json:"token_id"                                 gorm:"index"`
	TokenName                   string           `json:"token_name,omitempty"                     gorm:"size:128"`
	PricingCurrency             string           `json:"pricing_currency,omitempty"               gorm:"size:16"`
	PricingVersion              string           `json:"pricing_version,omitempty"                gorm:"size:128"`
	Price                       Price            `json:"price"                                    gorm:"embedded"`
	UpstreamID                  string           `json:"upstream_id"                              gorm:"type:varchar(256);index"`
	Status                      AsyncUsageStatus `json:"status"                                   gorm:"index;default:1"`
	Usage                       Usage            `json:"usage"                                    gorm:"embedded"`
	UsageContext                UsageContext     `json:"usage_context,omitempty"                  gorm:"embedded"`
	DisableResolutionFuzzyMatch bool             `json:"disable_resolution_fuzzy_match,omitempty"`
	Amount                      Amount           `json:"amount,omitempty"                         gorm:"embedded"`
	Error                       string           `json:"error,omitempty"                          gorm:"type:text"`
	RetryCount                  int              `json:"retry_count"`
	BalanceConsumeAttempted     bool             `json:"balance_consume_attempted"`
	BalanceConsumed             bool             `json:"balance_consumed"`
	ProcessingToken             string           `json:"-"                                        gorm:"size:64;index"`
	NextPollAt                  time.Time        `json:"next_poll_at"                             gorm:"index"`
	CreatedAt                   time.Time        `json:"created_at"`
	UpdatedAt                   time.Time        `json:"updated_at"`
	LogID                       int              `json:"-"`
	ImageTaskID                 string           `json:"-"                                        gorm:"size:128;index"`
	// Key usage P2 attribution (migrations/usage-attribution-v1). Every info a
	// P2 binary creates sets all three (SetUsageIdentity); NULL marks an info
	// created by an earlier binary. usage_at_ms is the request start; request_at
	// stays the pricing instant (plan C8).
	UsageModelID EmptyNullString `json:"-" gorm:"type:varchar(256)"`
	UsageSource  EmptyNullString `json:"-" gorm:"type:varchar(16)"`
	UsageAtMs    ZeroNullInt64   `json:"-" gorm:"type:bigint"`
}

func CreateAsyncUsageInfo(info *AsyncUsageInfo) error {
	if info.Capability != "" {
		capability := ModelCapability(info.Capability)
		// This is persistence after the distributor has authorized the model's
		// compiled contract. Reference is a v2 capability; do not expand the
		// legacy v1 ModelCapability.Valid permission surface to store its usage.
		if !capability.Valid() && info.Capability != "reference-to-video" {
			return fmt.Errorf("invalid async usage capability %q", info.Capability)
		}

		if strings.Contains(info.Model, modelCapabilityKeySeparator) {
			return errors.New("async usage model must be a public model ID")
		}
	}

	info.Status = AsyncUsageStatusPending
	info.CreatedAt = time.Now()

	info.UpdatedAt = info.CreatedAt
	if info.NextPollAt.IsZero() {
		info.NextPollAt = info.CreatedAt.Add(AsyncUsageDefaultPollDelay)
	}

	return LogDB.Create(info).Error
}

func GetPendingAsyncUsages(limit int) ([]*AsyncUsageInfo, error) {
	return GetPendingAsyncUsagesDue(limit, time.Now())
}

func FindCompletedAsyncUsageByUpstreamID(
	groupID string,
	tokenID int,
	upstreamID string,
) (*AsyncUsageInfo, error) {
	if LogDB == nil || groupID == "" || tokenID == 0 || upstreamID == "" {
		return nil, nil
	}

	var info AsyncUsageInfo

	err := LogDB.
		Where("group_id = ?", groupID).
		Where("token_id = ?", tokenID).
		Where("upstream_id = ?", upstreamID).
		Where("status = ?", int(AsyncUsageStatusCompleted)).
		Order("updated_at DESC, id DESC").
		First(&info).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return &info, nil
}

func GetPendingAsyncUsagesDue(
	limit int,
	now time.Time,
) ([]*AsyncUsageInfo, error) {
	var infos []*AsyncUsageInfo

	err := LogDB.
		Where("status = ?", int(AsyncUsageStatusPending)).
		Where(
			LogDB.
				Where("next_poll_at <= ?", now).
				Or("next_poll_at IS NULL"),
		).
		Order("next_poll_at ASC, updated_at ASC, created_at ASC").
		Limit(limit).
		Find(&infos).Error

	return infos, err
}

func TryClaimAsyncUsageInfo(
	info *AsyncUsageInfo,
	token string,
	leaseUntil time.Time,
	now time.Time,
) (bool, error) {
	if info == nil || info.ID == 0 || token == "" {
		return false, nil
	}

	tx := LogDB.
		Model(&AsyncUsageInfo{}).
		Where("id = ? AND status = ?", info.ID, int(AsyncUsageStatusPending)).
		Where(
			LogDB.
				Where("next_poll_at <= ?", now).
				Or("next_poll_at IS NULL"),
		).
		Updates(map[string]any{
			"processing_token": token,
			"next_poll_at":     leaseUntil,
			"updated_at":       now,
		})
	if tx.Error != nil {
		return false, tx.Error
	}

	if tx.RowsAffected == 0 {
		return false, nil
	}

	info.ProcessingToken = token
	info.NextPollAt = leaseUntil
	info.UpdatedAt = now

	return true, nil
}

func RenewAsyncUsageClaim(
	id int,
	token string,
	leaseUntil time.Time,
) (bool, error) {
	if id == 0 || token == "" {
		return false, nil
	}

	now := time.Now()

	tx := LogDB.
		Model(&AsyncUsageInfo{}).
		Where("id = ? AND status = ? AND processing_token = ?", id, int(AsyncUsageStatusPending), token).
		Updates(map[string]any{
			"next_poll_at": leaseUntil,
			"updated_at":   now,
		})
	if tx.Error != nil {
		return false, tx.Error
	}

	return tx.RowsAffected > 0, nil
}

func AsyncUsageBackoffDelay(
	retryCount int,
) time.Duration {
	if retryCount <= 1 {
		return AsyncUsageDefaultPollDelay
	}

	delay := AsyncUsageDefaultPollDelay
	for range retryCount - 1 {
		delay *= 2
		if delay >= AsyncUsageMaxPollDelay {
			return AsyncUsageMaxPollDelay
		}
	}

	return delay
}

func UpdateAsyncUsageInfo(info *AsyncUsageInfo) error {
	info.UpdatedAt = time.Now()
	return LogDB.Save(info).Error
}

func MarkAsyncUsageBalanceConsumed(info *AsyncUsageInfo) error {
	return updateClaimedAsyncUsageInfo(info, map[string]any{
		"balance_consumed": true,
	})
}

func MarkAsyncUsageBalanceConsumeAttempted(info *AsyncUsageInfo) error {
	return updateClaimedAsyncUsageInfo(info, map[string]any{
		"balance_consume_attempted": true,
	})
}

func PrepareClaimedAsyncUsageSettlement(
	info *AsyncUsageInfo,
	usage Usage,
	usageContext UsageContext,
	amount Amount,
) error {
	updatesModel := &AsyncUsageInfo{
		Usage:        usage,
		UsageContext: usageContext,
		Amount:       amount,
	}

	updates, err := asyncUsageUpdateValues(
		updatesModel,
		"Usage",
		"UsageContext",
		"Amount",
	)
	if err != nil {
		return err
	}

	return updateClaimedAsyncUsageInfo(info, updates)
}

func RetryClaimedAsyncUsageInfo(info *AsyncUsageInfo) error {
	return updateClaimedAsyncUsageInfo(info, map[string]any{
		"retry_count":      info.RetryCount,
		"error":            info.Error,
		"next_poll_at":     info.NextPollAt,
		"processing_token": "",
	})
}

func TouchClaimedAsyncUsageInfo(info *AsyncUsageInfo) error {
	return updateClaimedAsyncUsageInfo(info, map[string]any{
		"error":            "",
		"next_poll_at":     info.NextPollAt,
		"processing_token": "",
	})
}

func FailClaimedAsyncUsageInfo(info *AsyncUsageInfo) (bool, error) {
	tx := LogDB.
		Model(&AsyncUsageInfo{}).
		Where("id = ? AND processing_token = ?", info.ID, info.ProcessingToken).
		Updates(map[string]any{
			"status":           int(AsyncUsageStatusFailed),
			"error":            info.Error,
			"processing_token": "",
			"updated_at":       time.Now(),
		})
	if tx.Error != nil {
		return false, tx.Error
	}

	return tx.RowsAffected > 0, nil
}

func CompleteClaimedAsyncUsageInfo(
	info *AsyncUsageInfo,
	usage Usage,
	usageContext UsageContext,
	amount Amount,
) (bool, error) {
	now := time.Now()
	updatesModel := &AsyncUsageInfo{
		Status:          AsyncUsageStatusCompleted,
		Usage:           usage,
		UsageContext:    usageContext,
		Amount:          amount,
		Error:           "",
		BalanceConsumed: info.BalanceConsumed,
		ProcessingToken: "",
		UpdatedAt:       now,
	}

	updates, err := asyncUsageUpdateValues(
		updatesModel,
		"Status",
		"Usage",
		"UsageContext",
		"Amount",
		"Error",
		"BalanceConsumed",
		"ProcessingToken",
		"UpdatedAt",
	)
	if err != nil {
		return false, err
	}

	tx := LogDB.
		Model(&AsyncUsageInfo{}).
		Where("id = ? AND processing_token = ?", info.ID, info.ProcessingToken).
		Updates(updates)
	if tx.Error != nil {
		return false, tx.Error
	}

	return tx.RowsAffected > 0, nil
}

func asyncUsageUpdateValues(
	info *AsyncUsageInfo,
	names ...string,
) (map[string]any, error) {
	if info == nil {
		return nil, errors.New("async usage info is nil")
	}

	var namer schema.Namer = schema.NamingStrategy{IdentifierMaxLength: 64}
	if LogDB != nil && LogDB.NamingStrategy != nil {
		namer = LogDB.NamingStrategy
	}

	s, err := schema.Parse(&AsyncUsageInfo{}, &asyncUsageSchemaCache, namer)
	if err != nil {
		return nil, fmt.Errorf("parse async usage schema: %w", err)
	}

	values := make(map[string]any)
	reflectValue := reflect.ValueOf(info)
	ctx := context.Background()
	selected := make(map[string]struct{}, len(names))
	seen := make(map[string]struct{}, len(names))

	for _, name := range names {
		selected[name] = struct{}{}
	}

	for _, field := range s.Fields {
		if !field.Updatable || field.DBName == "" || len(field.BindNames) == 0 {
			continue
		}

		topName := field.BindNames[0]
		if _, ok := selected[topName]; !ok {
			continue
		}

		value, _ := field.ValueOf(ctx, reflectValue)
		values[field.DBName] = value
		seen[topName] = struct{}{}
	}

	for _, name := range names {
		if _, ok := seen[name]; !ok {
			return nil, fmt.Errorf("async usage field %q not found", name)
		}
	}

	return values, nil
}

func updateClaimedAsyncUsageInfo(
	info *AsyncUsageInfo,
	updates map[string]any,
) error {
	if info == nil || info.ProcessingToken == "" {
		return NotFoundError("async usage claim")
	}

	updates["updated_at"] = time.Now()

	tx := LogDB.
		Model(&AsyncUsageInfo{}).
		Where("id = ? AND processing_token = ?", info.ID, info.ProcessingToken).
		Updates(updates)
	if tx.Error != nil {
		return tx.Error
	}

	if tx.RowsAffected == 0 {
		return NotFoundError("async usage claim")
	}

	return nil
}

// asyncUsageLogs selects the call row an async usage outbox settles by the
// pre-P2 key. The request id is caller-chosen (X-Request-ID) and unique neither
// across groups nor within one, so the row is always narrowed to the identity
// the outbox captured from the same request as the row: its group, token and
// mode. Like nativeTaskLogs, rows without an async usage status (gateway
// rejections and failed submissions that reused the request id) are never
// settlement targets. An outbox without a token or mode keeps the remaining
// keys and never leaves its own group. Accounting outboxes (measured images and
// image tasks) are bound to their own row id; without one they select nothing,
// because every attempt of one request shares the other keys.
func asyncUsageLogs(info *AsyncUsageInfo) (*gorm.DB, bool) {
	if info == nil || info.RequestID == "" {
		return nil, false
	}

	if requiresAsyncUsageLogID(info) && info.LogID <= 0 {
		return nil, false
	}

	query := LogDB.Model(&Log{}).Where(
		"request_id = ? AND group_id = ? AND async_usage_status <> ?",
		info.RequestID,
		info.GroupID,
		AsyncUsageStatusNone,
	)
	if info.TokenID != 0 {
		query = query.Where("token_id = ?", info.TokenID)
	}

	if info.Mode != 0 {
		query = query.Where("mode = ?", info.Mode)
	}

	if info.LogID > 0 {
		query = query.Where("id = ?", info.LogID)
	}

	return query, true
}

func requiresAsyncUsageLogID(info *AsyncUsageInfo) bool {
	return info.MeasuredImage || info.ImageTaskID != ""
}

// resolveAsyncUsageLog finds the one call row an outbox settles; every update
// then targets that row id only (key usage P2, plan §1.6).
//
//  1. An outbox with an op finds the row that carries the same op in its group.
//  2. Only an info created before P2 (no usage columns) falls back to the
//     pre-P2 key, and then only to a row without an op: a pre-P2 row in flight
//     at the switch still settles, and another op's row is never touched.
//  3. A P2 info never falls back. Its own row always carries the op, so a miss
//     is gorm.ErrRecordNotFound: the pre-P2 key also matches completed rows of
//     an older call that reused the X-Request-Id, and settling onto one would
//     overwrite that call's usage and amount.
func resolveAsyncUsageLog(info *AsyncUsageInfo) (int, error) {
	if info == nil || (info.RequestID == "" && info.BillingOperationID == "") {
		return 0, NotFoundError("log")
	}

	if requiresAsyncUsageLogID(info) && info.LogID <= 0 {
		return 0, NotFoundError("log")
	}

	var ids []int

	if info.BillingOperationID != "" {
		query := LogDB.Model(&Log{}).Where(
			"group_id = ? AND billing_operation_id = ? AND async_usage_status <> ?",
			info.GroupID,
			info.BillingOperationID,
			AsyncUsageStatusNone,
		)
		if info.LogID > 0 {
			query = query.Where("id = ?", info.LogID)
		}

		if err := query.Order("id").Limit(1).Pluck("id", &ids).Error; err != nil {
			return 0, err
		}

		if len(ids) == 1 {
			return ids[0], nil
		}
	}

	if _, p2 := info.UsageIdentity(); p2 {
		return 0, NotFoundError("log")
	}

	query, ok := asyncUsageLogs(info)
	if !ok {
		return 0, NotFoundError("log")
	}

	if err := query.Where("billing_operation_id IS NULL").Order("id").Limit(1).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}

	if len(ids) != 1 {
		return 0, NotFoundError("log")
	}

	return ids[0], nil
}

func UpdateAsyncUsageLogUsage(
	info *AsyncUsageInfo,
	usage Usage,
	usageContext UsageContext,
	price Price,
	amount Amount,
	currency string,
	pricingVersion string,
) error {
	logID, err := resolveAsyncUsageLog(info)
	if err != nil {
		return err
	}

	var logEntry Log
	if err := LogDB.Where("id = ?", logID).First(&logEntry).Error; err != nil {
		return err
	}

	logEntry.Usage = usage
	logEntry.UsageContext = usageContext
	logEntry.Price = price
	logEntry.Amount = amount
	logEntry.Currency = currency
	logEntry.PricingVersion = pricingVersion
	logEntry.AsyncUsageStatus = AsyncUsageStatusCompleted

	return LogDB.Save(&logEntry).Error
}

func UpdateAsyncUsageLogStatus(
	info *AsyncUsageInfo,
	status AsyncUsageStatus,
) error {
	if info == nil || info.RequestID == "" {
		return nil
	}

	logID, err := resolveAsyncUsageLog(info)
	if err != nil {
		return err
	}

	tx := LogDB.Model(&Log{}).Where("id = ?", logID).Update("async_usage_status", status)
	if tx.Error != nil {
		return tx.Error
	}

	if tx.RowsAffected == 0 {
		return NotFoundError("log")
	}

	return nil
}

func UpdateAsyncUsageLogFailed(info *AsyncUsageInfo, message string) error {
	if info == nil || info.RequestID == "" {
		return nil
	}

	logID, err := resolveAsyncUsageLog(info)
	if err != nil {
		return err
	}

	tx := LogDB.Model(&Log{}).Where("id = ?", logID).Updates(
		map[string]any{"async_usage_status": AsyncUsageStatusFailed, "content": message},
	)

	if tx.Error != nil {
		return tx.Error
	}

	if tx.RowsAffected == 0 {
		return NotFoundError("log")
	}

	return nil
}

func CleanupFinishedAsyncUsages(olderThan time.Duration, batchSize int) error {
	if batchSize <= 0 {
		batchSize = defaultCleanLogBatchSize
	}

	cutoff := time.Now().Add(-olderThan)

	subQuery := LogDB.
		Model(&AsyncUsageInfo{}).
		Where(
			"status IN (?) AND updated_at < ?",
			[]AsyncUsageStatus{AsyncUsageStatusCompleted, AsyncUsageStatusFailed},
			cutoff,
		).
		Limit(batchSize).
		Select("id")

	return LogDB.
		Session(&gorm.Session{SkipDefaultTransaction: true}).
		Where("id IN (?)", subQuery).
		Delete(&AsyncUsageInfo{}).Error
}
