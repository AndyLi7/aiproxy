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
	// BillingOperationID is the op a call row records (key usage P2). The
	// relay sets it on every row except reads; rows that cannot move money
	// leave it empty (NULL).
	BillingOperationID string
}

// AdminLogKind splits the admin request log (/api/logs/ and /api/logs/search)
// into two disjoint lists; the empty value and "all" list every row. The task
// view (C step 2, see task_view.go) is a second split of every row: tasks
// (one row per generation task, trials and admin demos included) and
// non_task (every other row, unidentified callers included).
//
//   - calls: every row whose key was recognised (group and token known) and
//     that is not a read. Unlike GroupLogKindCalls it keeps internal demo and
//     trial rows (request_source admin_demo): the admin looks for them among
//     calls. Rejected submits, failed calls and task rows are calls.
//   - reads: every other row. That is a relay read mode (polls, downloads,
//     deletes and other reads; see IsReadMode), a gateway rejection of a GET or
//     DELETE request (a failed task status poll or artifact download, a
//     models list refused), and a request whose key was not recognised, such
//     as a 401, which no customer owns.
type AdminLogKind string

const (
	AdminLogKindAll     AdminLogKind = "all"
	AdminLogKindCalls   AdminLogKind = "calls"
	AdminLogKindReads   AdminLogKind = "reads"
	AdminLogKindTasks   AdminLogKind = "tasks"
	AdminLogKindNonTask AdminLogKind = "non_task"

	// AdminLogKindFeature tells the admin app that /api/logs/ and
	// /api/logs/search honour kind; an older gateway ignores the parameter
	// and returns every row.
	AdminLogKindFeature = "admin_log_kind_v1"
)

func (k AdminLogKind) Valid() bool {
	switch k {
	case "", AdminLogKindAll, AdminLogKindCalls, AdminLogKindReads, AdminLogKindTasks, AdminLogKindNonTask:
		return true
	default:
		return false
	}
}

type OperationalLogFilter struct {
	Status        CallStatus
	Source        string
	ChannelIDs    []int
	ExcludedModes []int
	// Kind applies to the admin request log only; see AdminLogKind.
	Kind AdminLogKind
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

	now := filter.Now
	if now.IsZero() {
		now = time.Now()
	}

	if filter.Status != "" {
		statusSQL, statusArgs := callStatusExpr(now)
		tx = tx.Where("("+statusSQL+") = ?", append(statusArgs, string(filter.Status))...)
	}

	switch filter.Kind {
	case AdminLogKindCalls:
		callSQL, callArgs := adminLogCallExpr()
		tx = tx.Where("("+callSQL+")", callArgs...)
	case AdminLogKindReads:
		callSQL, callArgs := adminLogCallExpr()
		tx = tx.Where("NOT ("+callSQL+")", callArgs...)
	case AdminLogKindTasks:
		taskSQL, taskArgs := taskViewTaskExpr(true, now)
		tx = tx.Where("("+taskSQL+")", taskArgs...)
	case AdminLogKindNonTask:
		nonTaskSQL, nonTaskArgs := taskViewNonTaskExpr(true, now)
		tx = tx.Where("("+nonTaskSQL+")", nonTaskArgs...)
	}

	return tx
}

func RecordOperationalLog(entry *Log) error {
	return LogDB.Create(entry).Error
}
