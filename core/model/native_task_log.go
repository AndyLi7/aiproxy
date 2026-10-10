package model

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
)

// NativeTaskLog carries the request-log fields known when a native task is
// submitted. Native tasks bypass the relay consume path, so without this row a
// successful task never reached the request log; only gateway rejections did.
type NativeTaskLog struct {
	RequestAt     time.Time
	TokenName     string
	Endpoint      string
	RequestSource string
	IP            string
	Mode          int
	// RequestedModel is the model ID the request used. The row's model and
	// capability always come from the task's capability ID; a different
	// requested ID (public_api_id or an alias) is kept as requested_model.
	RequestedModel string
}

// nativeLogIdentity splits a frozen registry id such as
// "vendor/model/capability" into the public model and its capability.
func nativeLogIdentity(registryID string) (string, string) {
	if strings.Count(registryID, "/") < 2 {
		return registryID, ""
	}

	i := strings.LastIndex(registryID, "/")

	return registryID[:i], registryID[i+1:]
}

// nativeTaskLogs selects the task's own row. Gateway rejections for the same
// request id are separate rows and never carry an async usage status, and an
// image or video task that reused the request id has a different mode.
func nativeTaskLogs(tx *gorm.DB, id, group string, token int) *gorm.DB {
	return tx.Model(&Log{}).Where(
		"request_id = ? AND group_id = ? AND token_id = ? AND mode = ? AND async_usage_status <> ?",
		id, group, token, int(mode.NativeTasks), AsyncUsageStatusNone,
	)
}

func truncateNativeLogField(value string, limit int) string {
	if len(value) <= limit {
		return value
	}

	return value[:limit]
}

// RecordNativeTaskLog writes one pending row once the task holds a wallet claim
// and is about to be submitted upstream. Replays never add a second row.
func RecordNativeTaskLog(db *gorm.DB, task *NativeTask, info NativeTaskLog) error {
	if db == nil || task == nil || !db.Migrator().HasTable(&Log{}) {
		return nil
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := nativeTaskLogs(tx, task.ID, task.GroupID, task.TokenID).Count(&count).Error; err != nil {
			return err
		}

		if count > 0 {
			return nil
		}

		publicModel, capability := nativeLogIdentity(task.Model)

		requestAt := info.RequestAt
		if requestAt.IsZero() {
			requestAt = time.Now()
		}

		source := info.RequestSource
		if !ValidRequestSource(source) || source == "" {
			source = RequestSourceAPI
		}

		// Customer pages show an amount only when it is explicitly priced in USD.
		currency, pricingVersion := "", ""
		if quote, err := ParseImagePrepaymentQuote(task.PrepaymentQuoteJSON); err == nil {
			currency, pricingVersion = quote.Currency, truncateNativeLogField("native:"+quote.QuoteVersion, 128)
		}

		var metadata map[string]string
		if info.RequestedModel != "" && info.RequestedModel != task.Model {
			metadata = map[string]string{
				"requested_model": truncateNativeLogField(info.RequestedModel, MaxPublicModelIDLength),
			}
		}

		return tx.Create(&Log{
			Metadata:         metadata,
			RequestID:        EmptyNullString(task.ID),
			RequestAt:        requestAt,
			GroupID:          task.GroupID,
			TokenID:          task.TokenID,
			TokenName:        truncateNativeLogField(info.TokenName, 32),
			Model:            truncateNativeLogField(publicModel, 128),
			Capability:       truncateNativeLogField(capability, 64),
			ChannelID:        task.ChannelID,
			Mode:             info.Mode,
			Code:             202,
			Endpoint:         EmptyNullString(truncateNativeLogField(info.Endpoint, 64)),
			RequestSource:    source,
			Currency:         currency,
			PricingVersion:   pricingVersion,
			IP:               EmptyNullString(truncateNativeLogField(info.IP, 45)),
			AsyncUsageStatus: AsyncUsageStatusPending,
		}).Error
	})
}

// nativeSafeErrorLimit bounds the request-log summary of rejected parameters.
const nativeSafeErrorLimit = 256

// nativeTaskSafeError is the request-log summary of a failed task. Rejected
// parameters list only field paths and rule codes, e.g. "Invalid parameters:
// voice (unsupported_value)"; issues that do not fit are left out.
func nativeTaskSafeError(task *NativeTask) string {
	issues := NativeTaskIssues(task)
	if len(issues) == 0 {
		return "Native task failed"
	}
	summary := "Invalid parameters:"
	for i, issue := range issues {
		part := " " + issue.Field + " (" + issue.Rule + ")"
		if i > 0 {
			part = "," + part
		}
		if len(summary)+len(part) > nativeSafeErrorLimit {
			if i == 0 {
				return truncateNativeLogField(summary+part, nativeSafeErrorLimit)
			}
			break
		}
		summary += part
	}
	return summary
}

// syncNativeTaskLog mirrors the task's progress and wallet receipt into its
// request-log row. It is idempotent and runs on every billing sync.
func syncNativeTaskLog(tx *gorm.DB, id, group string, token int, receipt string) error {
	if !tx.Migrator().HasTable(&Log{}) {
		return nil
	}

	var task NativeTask
	if err := tx.Select("status", "upstream_id", "error_code", "public_error").
		Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).
		First(&task).Error; err != nil {
		return err
	}

	changes := map[string]any{}
	if task.UpstreamID != "" {
		changes["upstream_id"] = task.UpstreamID
	}

	switch task.Status {
	case "completed":
		changes["async_usage_status"] = AsyncUsageStatusCompleted
	case "failed":
		changes["async_usage_status"] = AsyncUsageStatusFailed
		changes["code"] = 502
		changes["error_code"] = truncateNativeLogField(task.ErrorCode, 64)
		changes["safe_error"] = nativeTaskSafeError(&task)
	}

	var parsed struct {
		Status        string `json:"status"`
		ChargedMicros *int64 `json:"chargedMicros"`
	}
	if json.Unmarshal([]byte(receipt), &parsed) == nil && parsed.ChargedMicros != nil &&
		(parsed.Status == "settled" || parsed.Status == "estimated" || parsed.Status == "refunded") {
		changes["used_amount"] = float64(*parsed.ChargedMicros) / 1e6
	}

	if len(changes) == 0 {
		return nil
	}

	return nativeTaskLogs(tx, id, group, token).Updates(changes).Error
}
