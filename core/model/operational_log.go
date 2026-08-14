package model

import (
	"regexp"
	"strings"

	"gorm.io/gorm"
)

type OperationalStatus string

const (
	OperationalStatusProcessing OperationalStatus = "processing"
	OperationalStatusSuccess    OperationalStatus = "success"
	OperationalStatusFailed     OperationalStatus = "failed"
	OperationalStatusRejected   OperationalStatus = "rejected"
)

type FailureStage string

const (
	FailureStageNone        FailureStage = ""
	FailureStageAuth        FailureStage = "auth"
	FailureStageEntitlement FailureStage = "entitlement"
	FailureStageModel       FailureStage = "model"
	FailureStageBalance     FailureStage = "balance"
	FailureStageValidation  FailureStage = "validation"
	FailureStageRateLimit   FailureStage = "rate_limit"
	FailureStageRouting     FailureStage = "routing"
	FailureStageUpstream    FailureStage = "upstream"
)

const (
	RequestSourceAPI        = "api"
	RequestSourcePlayground = "playground"
	maxSafeErrorRunes       = 512
)

type OperationalFields struct {
	RequestSource string
	FailureStage  FailureStage
	SafeError     string
}

type OperationalLogFilter struct {
	Status     OperationalStatus
	ChannelIDs []int
}

func (s OperationalStatus) Valid() bool {
	switch s {
	case "",
		OperationalStatusProcessing,
		OperationalStatusSuccess,
		OperationalStatusFailed,
		OperationalStatusRejected:
		return true
	default:
		return false
	}
}

var bearerCredentialPattern = regexp.MustCompile(`(?i)bearer\s+\S+`)

func BuildOperationalFields(
	requestSource string,
	failureStage FailureStage,
	safeError string,
) OperationalFields {
	switch requestSource {
	case RequestSourcePlayground:
	default:
		requestSource = RequestSourceAPI
	}

	if !failureStage.Valid() {
		failureStage = FailureStageNone
	}

	safeError = bearerCredentialPattern.ReplaceAllString(safeError, "[REDACTED]")
	safeError = strings.Join(strings.Fields(safeError), " ")
	runes := []rune(safeError)
	if len(runes) > maxSafeErrorRunes {
		safeError = string(runes[:maxSafeErrorRunes])
	}

	return OperationalFields{
		RequestSource: requestSource,
		FailureStage:  failureStage,
		SafeError:     safeError,
	}
}

func (s FailureStage) Valid() bool {
	switch s {
	case FailureStageNone,
		FailureStageAuth,
		FailureStageEntitlement,
		FailureStageModel,
		FailureStageBalance,
		FailureStageValidation,
		FailureStageRateLimit,
		FailureStageRouting,
		FailureStageUpstream:
		return true
	default:
		return false
	}
}

func (l *Log) OperationalStatus() OperationalStatus {
	if l.AsyncUsageStatus == AsyncUsageStatusFailed {
		return OperationalStatusFailed
	}

	if l.Code == 200 {
		if l.AsyncUsageStatus == AsyncUsageStatusPending {
			return OperationalStatusProcessing
		}

		return OperationalStatusSuccess
	}

	if l.FailureStage != FailureStageNone && l.FailureStage != FailureStageUpstream {
		return OperationalStatusRejected
	}

	return OperationalStatusFailed
}

func applyOperationalLogFilter(tx *gorm.DB, filter OperationalLogFilter) *gorm.DB {
	if len(filter.ChannelIDs) > 0 {
		tx = tx.Where("channel_id IN ?", filter.ChannelIDs)
	}

	switch filter.Status {
	case OperationalStatusProcessing:
		tx = tx.Where("code = 200 AND async_usage_status = ?", AsyncUsageStatusPending)
	case OperationalStatusSuccess:
		tx = tx.Where(
			"code = 200 AND async_usage_status != ? AND async_usage_status != ?",
			AsyncUsageStatusPending,
			AsyncUsageStatusFailed,
		)
	case OperationalStatusFailed:
		tx = tx.Where(
			"async_usage_status = ? OR (code != 200 AND (failure_stage = '' OR failure_stage = ?))",
			AsyncUsageStatusFailed,
			FailureStageUpstream,
		)
	case OperationalStatusRejected:
		tx = tx.Where(
			"code != 200 AND failure_stage != '' AND failure_stage != ?",
			FailureStageUpstream,
		)
	}

	return tx
}
