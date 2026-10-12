package model

import (
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
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
	RequestSourceAdminDemo  = "admin_demo"
	AdminDemoFeature        = "token_platform.admin_demo.v1"
	maxSafeErrorRunes       = 512
)

type OperationalFields struct {
	RequestSource         string
	FailureStage          FailureStage
	ErrorCode             string
	SafeError             string
	RequestedModel        string
	PublicModel           string
	PublicCapabilityModel string
	ResolvedCapability    string
}

type OperationalLogFilter struct {
	Status        CallStatus
	Source        string
	ChannelIDs    []int
	ExcludedModes []int
	// Now is the instant the status filter classifies at; zero means time.Now().
	Now time.Time
}

func ValidRequestSource(source string) bool {
	switch source {
	case "", RequestSourceAPI, RequestSourcePlayground, RequestSourceAdminDemo:
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
	errorCodes ...string,
) OperationalFields {
	switch requestSource {
	case RequestSourcePlayground, RequestSourceAdminDemo:
	default:
		requestSource = RequestSourceAPI
	}

	if !failureStage.Valid() {
		failureStage = FailureStageNone
	}

	errorCode := ""
	if len(errorCodes) > 0 {
		errorCode = errorCodes[0]
	}

	errorCode = normalizeOperationalErrorCode(errorCode, failureStage)

	safeError = bearerCredentialPattern.ReplaceAllString(safeError, "[REDACTED]")
	safeError = strings.Join(strings.Fields(safeError), " ")

	runes := []rune(safeError)
	if len(runes) > maxSafeErrorRunes {
		safeError = string(runes[:maxSafeErrorRunes])
	}

	return OperationalFields{
		RequestSource: requestSource,
		FailureStage:  failureStage,
		ErrorCode:     errorCode,
		SafeError:     safeError,
	}
}

var operationalErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func normalizeOperationalErrorCode(errorCode string, stage FailureStage) string {
	errorCode = strings.ToLower(strings.TrimSpace(errorCode))
	if operationalErrorCodePattern.MatchString(errorCode) {
		return errorCode
	}

	switch stage {
	case FailureStageAuth:
		return "authentication_failed"
	case FailureStageEntitlement:
		return "access_denied"
	case FailureStageModel:
		return "model_unavailable"
	case FailureStageBalance:
		return "insufficient_balance"
	case FailureStageValidation:
		return "invalid_request"
	case FailureStageRateLimit:
		return "rate_limited"
	case FailureStageRouting:
		return "no_available_route"
	case FailureStageUpstream:
		return "upstream_error"
	default:
		return ""
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

func applyOperationalLogFilter(tx *gorm.DB, filter OperationalLogFilter) *gorm.DB {
	if filter.Source != "" {
		tx = tx.Where("request_source = ?", filter.Source)
	}

	if len(filter.ChannelIDs) > 0 {
		tx = tx.Where("channel_id IN ?", filter.ChannelIDs)
	}

	if len(filter.ExcludedModes) > 0 {
		tx = tx.Where("mode IS NULL OR mode NOT IN ?", filter.ExcludedModes)
	}

	if filter.Status != "" {
		now := filter.Now
		if now.IsZero() {
			now = time.Now()
		}

		statusSQL, statusArgs := callStatusExpr(now)
		tx = tx.Where("("+statusSQL+") = ?", append(statusArgs, string(filter.Status))...)
	}

	return tx
}

func RecordOperationalLog(entry *Log) error {
	return LogDB.Create(entry).Error
}
