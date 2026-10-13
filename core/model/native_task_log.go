package model

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// nativeLogSource is the source a native row records: a known source, else api.
func nativeLogSource(source string) string {
	if !ValidRequestSource(source) || source == "" {
		return RequestSourceAPI
	}

	return source
}

// nativeLogPricing is the currency and pricing version of the task's frozen
// quote. Customer pages show an amount only when it is explicitly priced in USD.
func nativeLogPricing(task *NativeTask) (string, string) {
	quote, err := ParseImagePrepaymentQuote(task.PrepaymentQuoteJSON)
	if err != nil {
		return "", ""
	}

	return quote.Currency, truncateNativeLogField("native:"+quote.QuoteVersion, 128)
}

// lockNativeTask takes the task row's write lock for the rest of the
// transaction. RecordNativeTaskLog and SaveNativeBillingTerminal both write the
// task's own row after it, so concurrent writers serialise on the task and the
// count-then-insert yields exactly one row (SQLite serialises writers itself).
func lockNativeTask(tx *gorm.DB, id, group string, token int) (*NativeTask, error) {
	var task NativeTask

	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "status", "error_code", "group_id", "token_id", "model", "channel_id", "prepayment_quote_json", "billing_operation_id").
		Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).
		First(&task).Error
	if err != nil {
		return nil, err
	}

	return &task, nil
}

// RecordNativeTaskLog writes one pending row once the task holds a wallet claim
// and is about to be submitted upstream. Replays never add a second row, and a
// row recovery already inserted (syncNativeTaskLog) is never duplicated: both
// writers count and insert under the task's row lock.
func RecordNativeTaskLog(db *gorm.DB, task *NativeTask, info NativeTaskLog) error {
	if db == nil || task == nil || !db.Migrator().HasTable(&Log{}) {
		return nil
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if _, err := lockNativeTask(tx, task.ID, task.GroupID, task.TokenID); err != nil {
			return err
		}

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

		currency, pricingVersion := nativeLogPricing(task)

		var metadata map[string]string
		if info.RequestedModel != "" && info.RequestedModel != task.Model {
			metadata = map[string]string{
				"requested_model": truncateNativeLogField(info.RequestedModel, MaxPublicModelIDLength),
			}
		}

		return tx.Create(&Log{
			Metadata:           metadata,
			RequestID:          EmptyNullString(task.ID),
			RequestAt:          requestAt,
			GroupID:            task.GroupID,
			TokenID:            task.TokenID,
			TokenName:          truncateNativeLogField(info.TokenName, 32),
			Model:              truncateNativeLogField(publicModel, 128),
			Capability:         truncateNativeLogField(capability, 64),
			ChannelID:          task.ChannelID,
			Mode:               info.Mode,
			Code:               202,
			Endpoint:           EmptyNullString(truncateNativeLogField(info.Endpoint, 64)),
			RequestSource:      nativeLogSource(info.RequestSource),
			Currency:           currency,
			PricingVersion:     pricingVersion,
			IP:                 EmptyNullString(truncateNativeLogField(info.IP, 45)),
			AsyncUsageStatus:   AsyncUsageStatusPending,
			BillingOperationID: EmptyNullString(task.BillingOperationID),
		}).Error
	})
}

// nativeReceiptUsage is the hold's attribution a stored wallet receipt carries
// (usageAtMs, usageSource). Receipts of holds admitted before key usage P2, or
// from an application without it, have neither.
type nativeReceiptUsage struct {
	Status        string `json:"status"`
	ChargedMicros *int64 `json:"chargedMicros"`
	UsageAtMs     *int64 `json:"usageAtMs"`
	UsageSource   string `json:"usageSource"`
}

func (r nativeReceiptUsage) usable() bool {
	return r.UsageAtMs != nil && *r.UsageAtMs > 0 && validUsageSource(r.UsageSource)
}

func parseNativeReceiptUsage(receipt string) (nativeReceiptUsage, bool) {
	var parsed nativeReceiptUsage
	if json.Unmarshal([]byte(receipt), &parsed) != nil {
		return nativeReceiptUsage{}, false
	}

	return parsed, true
}

// NativeRecoveryRowNeeded reports, outside any transaction, whether a billing
// sync may have to insert the task's missing call row. It lets the caller read
// the key's name from the main database before the LogDB transaction, and only
// when an insert is possible. The transaction re-checks under the task lock.
func NativeRecoveryRowNeeded(db *gorm.DB, id, group string, token int, receipt string) bool {
	if db == nil || !db.Migrator().HasTable(&Log{}) {
		return false
	}

	parsed, ok := parseNativeReceiptUsage(receipt)
	if !ok || !parsed.usable() {
		return false
	}

	var count int64
	if err := nativeTaskLogs(db, id, group, token).Count(&count).Error; err != nil {
		return false
	}

	return count == 0
}

// nativeRecoveryTokenName is the key name a recovery-inserted row records, read
// from the main database; "" when it cannot be read.
func nativeRecoveryTokenName(token int) string {
	if DB == nil || token <= 0 {
		return ""
	}

	owner, err := GetTokenByID(token)
	if err != nil || owner == nil {
		return ""
	}

	return string(owner.Name)
}

// insertMissingNativeTaskLog writes the call row of a submission whose own
// RecordNativeTaskLog never committed (its failure is only logged). It runs in
// the billing sync's transaction, under the task lock, and only when
//   - the task was not reserved before this sync: some request already
//     received the 202 for this submission (a refunded reservation that never
//     reached a provider is flipped to failed by the same sync and must not
//     gain a row), nor failed by recovery while reserved (NotStartedCode):
//     that task never reached a provider either;
//   - the task has no own row (request id, key, mode, async status);
//   - the receipt carries the hold's time and source (pre-P2 holds are left to
//     the P3 backfill).
//
// The row copies the hold's time and source, so it equals the payload identity.
func insertMissingNativeTaskLog(
	tx *gorm.DB,
	task *NativeTask,
	priorStatus string,
	receipt nativeReceiptUsage,
	tokenName string,
) error {
	if task == nil || priorStatus == "reserved" || task.ErrorCode == NotStartedCode || !receipt.usable() {
		return nil
	}

	var count int64
	if err := nativeTaskLogs(tx, task.ID, task.GroupID, task.TokenID).Count(&count).Error; err != nil {
		return err
	}

	if count > 0 {
		return nil
	}

	publicModel, capability := nativeLogIdentity(task.Model)
	requestAt := time.UnixMilli(*receipt.UsageAtMs).UTC()
	currency, pricingVersion := nativeLogPricing(task)

	endpoint := "POST /v1/model-tasks"
	if receipt.UsageSource == RequestSourceAdminDemo {
		endpoint = "POST /api/native-trials"
	}

	return tx.Create(&Log{
		Metadata:           map[string]string{"inserted_by": "native_recovery"},
		RequestID:          EmptyNullString(task.ID),
		RequestAt:          requestAt,
		CreatedAt:          requestAt,
		GroupID:            task.GroupID,
		TokenID:            task.TokenID,
		TokenName:          truncateNativeLogField(tokenName, 32),
		Model:              truncateNativeLogField(publicModel, 128),
		Capability:         truncateNativeLogField(capability, 64),
		ChannelID:          task.ChannelID,
		Mode:               int(mode.NativeTasks),
		Code:               202,
		Endpoint:           EmptyNullString(endpoint),
		RequestSource:      receipt.UsageSource,
		Currency:           currency,
		PricingVersion:     pricingVersion,
		AsyncUsageStatus:   AsyncUsageStatusPending,
		BillingOperationID: EmptyNullString(task.BillingOperationID),
	}).Error
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
// request-log row. It is idempotent and runs on every billing sync. locked is
// the task as read under its row lock before this sync changed it; a missing
// call row is inserted first (insertMissingNativeTaskLog).
func syncNativeTaskLog(
	tx *gorm.DB,
	id, group string,
	token int,
	receipt string,
	locked *NativeTask,
	tokenName string,
) error {
	if !tx.Migrator().HasTable(&Log{}) {
		return nil
	}

	if locked != nil {
		parsed, _ := parseNativeReceiptUsage(receipt)
		if err := insertMissingNativeTaskLog(tx, locked, locked.Status, parsed, tokenName); err != nil {
			return err
		}
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

	parsed, ok := parseNativeReceiptUsage(receipt)
	if ok && parsed.ChargedMicros != nil &&
		(parsed.Status == "settled" || parsed.Status == "estimated" || parsed.Status == "refunded") {
		changes["used_amount"] = float64(*parsed.ChargedMicros) / 1e6
	}

	if len(changes) == 0 {
		return nil
	}

	return nativeTaskLogs(tx, id, group, token).Updates(changes).Error
}
