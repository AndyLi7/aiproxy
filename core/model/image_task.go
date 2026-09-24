package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrImageTaskConflict = errors.New("request id already belongs to a different image request")

type ImageOutput struct {
	URLExpiresAt *time.Time `json:"url_expires_at,omitempty"`
	Stored       bool       `json:"stored,omitempty"`
	Width        *int64     `json:"width,omitempty"`
	Height       *int64     `json:"height,omitempty"`
	URL          string     `json:"url"`
	ContentType  string     `json:"content_type,omitempty"`
}
type ImageTaskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ImageTask and its accounting outbox live in LogDB so reservation/activation are atomic.
// These records are deliberately retained: removing them would permit paid resubmission.
type ImageTask struct {
	Phase              string     `gorm:"-" json:"phase,omitempty"`
	ResultAvailability string     `gorm:"-" json:"result_availability,omitempty"`
	RequestSummary     string     `gorm:"-" json:"-"`
	ArchiveRequired    bool       `gorm:"not null;default:false" json:"-"`
	QueuedAt           *time.Time `json:"queued_at,omitempty"`
	RunningAt          *time.Time `json:"running_at,omitempty"`
	ResultReceivedAt   *time.Time `json:"result_received_at,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	ResultExpiresAt    *time.Time `json:"result_expires_at,omitempty"`
	RetainUntil        *time.Time `json:"retention_guaranteed_until,omitempty"`

	Attempts           []ImageTaskAttempt `gorm:"serializer:json;type:text" json:"-"`
	RequestModel       string             `gorm:"size:128"                  json:"-"`
	ValidationContract string             `gorm:"type:text"                 json:"-"`
	KeyFingerprint     string             `gorm:"size:64"                   json:"-"`
	ChannelType        ChannelType        `                                 json:"-"`
	ExpectedImages     int                `                                 json:"-"`
	ID                 string             `gorm:"primaryKey;size:128"       json:"id"`
	Model              string             `gorm:"size:128"                  json:"model"`
	Status             string             `gorm:"size:32;index"             json:"status"`
	Data               []ImageOutput      `gorm:"serializer:json;type:text" json:"data,omitempty"`
	Error              *ImageTaskError    `gorm:"serializer:json;type:text" json:"error,omitempty"`
	GroupID            string             `gorm:"size:64;index"             json:"-"`
	TokenID            int                `                                 json:"-"`
	Fingerprint        string             `gorm:"size:64"                   json:"-"`
	UpstreamModel      string             `gorm:"size:256"                  json:"-"`
	UpstreamID         string             `gorm:"size:256"                  json:"-"`
	UsageID            int                `                                 json:"-"`
	CreatedAt          time.Time          `                                 json:"submitted_at"`
	UpdatedAt          time.Time          `                                 json:"-"`
}

func GetImageTask(id, group string, token int) (*ImageTask, error) {
	var task ImageTask

	err := LogDB.Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).
		First(&task).
		Error

	return &task, err
}

// Admin result recovery is scoped to the customer's group rather than an API
// token, so deleting or rotating a key does not hide a completed generation.
func GetGroupImageTask(id, group string) (*ImageTask, error) {
	var task ImageTask
	err := LogDB.Where("id = ? AND group_id = ?", id, group).First(&task).Error
	return &task, err
}

func ReserveImageTask(
	task *ImageTask,
	info *AsyncUsageInfo,
	operational ...OperationalFields,
) (*ImageTask, bool, error) {
	created := false
	var reservationLogID int
	err := LogDB.Transaction(func(tx *gorm.DB) error {
		task.Status = "submitting"
		now := time.Now().UTC()
		task.CreatedAt = now
		retain := now.Add(30 * 24 * time.Hour)
		task.RetainUntil = &retain

		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(task)
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected == 0 {
			var existing ImageTask
			if err := tx.First(&existing, "id = ?", task.ID).Error; err != nil {
				return err
			}

			if existing.GroupID != task.GroupID || existing.TokenID != task.TokenID ||
				existing.Model != task.Model ||
				existing.Fingerprint != task.Fingerprint {
				return ErrImageTaskConflict
			}

			*task = existing

			return nil
		}

		var prior []Log
		if err := tx.Select("group_id", "token_id", "model", "capability", "code", "error_code", "failure_stage", "endpoint", "channel_id", "upstream_id", "used_amount").
			Where("request_id = ?", task.ID).Limit(101).Find(&prior).Error; err != nil {
			return err
		}
		if len(prior) > 100 {
			return ErrImageTaskConflict
		}
		for _, previous := range prior {
			// A gateway-only endpoint rejection has not submitted a generation.
			// Preserve its audit log while allowing the same owner to switch endpoints.
			identity := previous.Model
			if previous.Capability != "" {
				identity += "/" + previous.Capability
			}
			if previous.GroupID != task.GroupID || previous.TokenID != task.TokenID ||
				identity != task.Model || previous.Code != 400 || previous.ErrorCode != "unsupported_endpoint" ||
				previous.FailureStage != FailureStageValidation ||
				(previous.Endpoint != "POST /v1/images/generations" && previous.Endpoint != "/v1/images/generations") ||
				previous.ChannelID != 0 || previous.UpstreamID != "" || previous.Amount.UsedAmount != 0 {
				return ErrImageTaskConflict
			}
		}

		entry := &Log{
			RequestID:        EmptyNullString(task.ID),
			RequestAt:        info.RequestAt,
			GroupID:          info.GroupID,
			TokenID:          info.TokenID,
			TokenName:        info.TokenName,
			Model:            info.Model,
			ChannelID:        info.ChannelID,
			Mode:             info.Mode,
			Code:             202,
			Endpoint:         "/v1/images/tasks",
			RequestSource:    RequestSourceAPI,
			Currency:         info.PricingCurrency,
			PricingVersion:   info.PricingVersion,
			Price:            info.Price,
			AsyncUsageStatus: AsyncUsageStatusPending,
		}
		if len(operational) > 0 {
			fields := operational[0]
			entry.RequestSource = fields.RequestSource
			entry.Model, entry.Capability = PublicLogIdentity(
				info.Model,
				fields.PublicModel,
				fields.ResolvedCapability,
			)
		}
		if err := tx.Create(entry).Error; err != nil {
			return err
		}
		reservationLogID = entry.ID
		// Pending starts only once an upstream id is durably attached. GORM's default
		// status is pending, so explicitly overwrite it within this transaction.
		info.ImageTaskID = task.ID

		info.LogID = entry.ID
		if err := tx.Create(info).Error; err != nil {
			return err
		}

		if err := tx.Model(info).Update("status", AsyncUsageStatusNone).Error; err != nil {
			return err
		}

		task.UsageID = info.ID
		if err := tx.Model(task).Update("usage_id", info.ID).Error; err != nil {
			return err
		}

		created = true

		return nil
	})
	if err == nil && created && task.RequestSummary != "" {
		if detailErr := LogDB.Create(&RequestDetail{LogID: reservationLogID, RequestBody: task.RequestSummary}).Error; detailErr != nil {
			log.WithError(detailErr).WithField("log_id", reservationLogID).Warn("save image request summary")
		}
	}

	return task, created, err
}

func AcceptImageTask(id, upstream string) error {
	return LogDB.Transaction(func(tx *gorm.DB) error {
		var task ImageTask
		if err := tx.First(&task, "id = ?", id).Error; err != nil {
			return err
		}

		if task.Status != "submitting" && task.Status != "submission_unknown" {
			return errors.New("image task already submitted")
		}

		if upstream == "" {
			return errors.New("missing upstream id")
		}

		if err := tx.Model(&task).
			Updates(map[string]any{"status": "queued", "upstream_id": upstream, "queued_at": gorm.Expr("COALESCE(queued_at, ?)", time.Now().UTC())}).
			Error; err != nil {
			return err
		}

		if err := tx.Model(&Log{}).
			Where("request_id = ? AND id IN (?)", id, tx.Model(&AsyncUsageInfo{}).Select("log_id").Where("image_task_id = ?", id)).
			Update("upstream_id", upstream).
			Error; err != nil {
			return err
		}

		return tx.Model(&AsyncUsageInfo{}).
			Where("id = ?", task.UsageID).
			Updates(map[string]any{"status": AsyncUsageStatusPending, "upstream_id": upstream, "next_poll_at": time.Now()}).
			Error
	})
}

func SetImageTaskResult(id, status string, data []ImageOutput, taskError *ImageTaskError) error {
	if status == "completed" && len(data) == 0 {
		return errors.New("empty completed image result")
	}

	encodedData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	encodedError, err := json.Marshal(taskError)
	if err != nil {
		return err
	}

	return LogDB.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		changes := map[string]any{"status": status, "data": string(encodedData), "error": string(encodedError), "updated_at": now}
		if status == "queued" {
			changes["queued_at"] = gorm.Expr("COALESCE(queued_at, ?)", now)
		}
		if status == "in_progress" {
			changes["running_at"] = gorm.Expr("COALESCE(running_at, ?)", now)
		}
		if status == "result_processing" || status == "completed" {
			changes["result_received_at"] = gorm.Expr("COALESCE(result_received_at, ?)", now)
		}
		if status == "completed" || status == "failed" {
			changes["completed_at"] = now
			changes["retain_until"] = now.Add(30 * 24 * time.Hour)
		}
		allowed := []string{"submitting", "submission_unknown", "queued", "in_progress"}
		if status == "submission_unknown" {
			allowed = []string{"submitting", "submission_unknown"}
		}
		if status == "queued" {
			allowed = []string{"submitting", "submission_unknown", "queued"}
		}
		if status != "submission_unknown" && status != "queued" && status != "in_progress" && status != "result_processing" && status != "completed" && status != "failed" {
			return errors.New("invalid image task status")
		}
		result := tx.Model(&ImageTask{}).
			Where("id = ? AND status IN ?", id, allowed).
			Updates(changes)
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected == 0 {
			return nil
		}

		if status == "failed" {
			if err := tx.Model(&AsyncUsageInfo{}).
				Where("image_task_id = ?", id).
				Update("status", AsyncUsageStatusFailed).
				Error; err != nil {
				return err
			}

			return tx.Model(&Log{}).
				Where("request_id = ? AND id IN (?)", id, tx.Model(&AsyncUsageInfo{}).Select("log_id").Where("image_task_id = ?", id)).
				Updates(map[string]any{"async_usage_status": AsyncUsageStatusFailed, "code": 502, "safe_error": "Image generation failed"}).
				Error
		}

		return nil
	})
}

// CheckImageChannelRetention protects accepted jobs when publication retires a
// channel. Unknown acceptance also retains credentials for operator recovery.
func CheckImageChannelRetention(ids []int) error {
	if LogDB == nil || !LogDB.Migrator().HasTable(&ImageTask{}) {
		return nil
	}

	var count int64

	err := LogDB.Model(&AsyncUsageInfo{}).
		Where("channel_id IN ? AND image_task_id <> ? AND status IN ?", ids, "", []AsyncUsageStatus{AsyncUsageStatusNone, AsyncUsageStatusPending}).
		Count(&count).
		Error
	if err != nil {
		return err
	}

	if count > 0 {
		return errors.New("channel retained by unsettled image tasks")
	}

	return nil
}

func ImageChannelKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func preserveImageAccountingLogs(tx *gorm.DB) *gorm.DB {
	if !LogDB.Migrator().HasTable(&ImageTask{}) {
		return tx
	}

	return tx.Where(
		"id NOT IN (?)",
		LogDB.Model(&AsyncUsageInfo{}).
			Select("log_id").
			Where("image_task_id <> ? AND status IN ?", "", []AsyncUsageStatus{AsyncUsageStatusNone, AsyncUsageStatusPending}),
	)
}

func RecoverStaleImageSubmissions(now time.Time) error {
	return LogDB.Model(&ImageTask{}).
		Where("status = ? AND updated_at < ?", "submitting", now.Add(-2*time.Minute)).
		Update("status", "submission_unknown").
		Error
}

// CompleteSyncImageTask commits the result and activates its accounting outbox
// together. A crash before this commit leaves the reservation unknown; it must
// never cause a second upstream submission.
func CompleteSyncImageTask(id string, data []ImageOutput) error {
	if len(data) == 0 {
		return errors.New("empty synchronous image result")
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return LogDB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ImageTask{}).
			Where("id = ? AND status = ?", id, "submitting").
			Updates(map[string]any{"status": "completed", "data": string(encoded), "updated_at": time.Now()})
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return errors.New("image reservation already resolved")
		}

		usage := tx.Model(&AsyncUsageInfo{}).
			Where("image_task_id = ? AND status = ?", id, AsyncUsageStatusNone).
			Updates(map[string]any{"status": AsyncUsageStatusPending, "next_poll_at": time.Now()})
		if usage.Error != nil {
			return usage.Error
		}

		if usage.RowsAffected != 1 {
			return errors.New("image accounting reservation missing")
		}

		return nil
	})
}
