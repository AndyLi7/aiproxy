package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrImageTaskConflict = errors.New("request id already belongs to a different image request")

type ImageOutput struct {
	Width       *int64 `json:"width,omitempty"`
	Height      *int64 `json:"height,omitempty"`
	URL         string `json:"url"`
	ContentType string `json:"content_type,omitempty"`
}
type ImageTaskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ImageTask and its accounting outbox live in LogDB so reservation/activation are atomic.
// These records are deliberately retained: removing them would permit paid resubmission.
type ImageTask struct {
	RequestModel       string          `gorm:"size:128"                  json:"-"`
	ValidationContract string          `gorm:"type:text"                 json:"-"`
	KeyFingerprint     string          `gorm:"size:64"                   json:"-"`
	ChannelType        ChannelType     `                                 json:"-"`
	ExpectedImages     int             `                                 json:"-"`
	ID                 string          `gorm:"primaryKey;size:128"       json:"id"`
	Model              string          `gorm:"size:128"                  json:"model"`
	Status             string          `gorm:"size:32;index"             json:"status"`
	Data               []ImageOutput   `gorm:"serializer:json;type:text" json:"data,omitempty"`
	Error              *ImageTaskError `gorm:"serializer:json;type:text" json:"error,omitempty"`
	GroupID            string          `gorm:"size:64;index"             json:"-"`
	TokenID            int             `                                 json:"-"`
	Fingerprint        string          `gorm:"size:64"                   json:"-"`
	UpstreamModel      string          `gorm:"size:256"                  json:"-"`
	UpstreamID         string          `gorm:"size:256"                  json:"-"`
	UsageID            int             `                                 json:"-"`
	CreatedAt          time.Time       `                                 json:"-"`
	UpdatedAt          time.Time       `                                 json:"-"`
}

func GetImageTask(id, group string, token int) (*ImageTask, error) {
	var task ImageTask

	err := LogDB.Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).
		First(&task).
		Error

	return &task, err
}

func ReserveImageTask(
	task *ImageTask,
	info *AsyncUsageInfo,
	operational ...OperationalFields,
) (*ImageTask, bool, error) {
	created := false
	err := LogDB.Transaction(func(tx *gorm.DB) error {
		task.Status = "submitting"

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

		var prior int64
		if err := tx.Model(&Log{}).
			Where("request_id = ?", task.ID).
			Count(&prior).
			Error; err != nil {
			return err
		}

		if prior > 0 {
			return ErrImageTaskConflict
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
			Updates(map[string]any{"status": "queued", "upstream_id": upstream}).
			Error; err != nil {
			return err
		}

		if err := tx.Model(&Log{}).
			Where("request_id = ?", id).
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
		result := tx.Model(&ImageTask{}).
			Where("id = ? AND status NOT IN ?", id, []string{"completed", "failed"}).
			Updates(map[string]any{"status": status, "data": string(encodedData), "error": string(encodedError), "updated_at": time.Now()})
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
				Where("request_id = ?", id).
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
		result := tx.Model(&ImageTask{}).Where("id = ? AND status = ?", id, "submitting").Updates(map[string]any{"status": "completed", "data": string(encoded), "updated_at": time.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("image reservation already resolved")
		}
		usage := tx.Model(&AsyncUsageInfo{}).Where("image_task_id = ? AND status = ?", id, AsyncUsageStatusNone).Updates(map[string]any{"status": AsyncUsageStatusPending, "next_poll_at": time.Now()})
		if usage.Error != nil {
			return usage.Error
		}
		if usage.RowsAffected != 1 {
			return errors.New("image accounting reservation missing")
		}
		return nil
	})
}
