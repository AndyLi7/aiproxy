package middleware

import (
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

const OperationalLogSourceHeader = "X-Token-Platform-Source"

const (
	operationalFailureStageKey = "operational_failure_stage"
	operationalErrorCodeKey    = "operational_error_code"
	operationalSafeErrorKey    = "operational_safe_error"
	operationalLogRecordedKey  = "operational_log_recorded"
	operationalLogIdentityKey  = "operational_log_identity"
	callBillingOperationKey    = "call_billing_operation_id"
)

// operationalLogIdentity names the key TokenAuth identified, for the request
// log only. It is kept apart from Token and Group so nothing downstream treats
// a rejected key as authenticated.
type operationalLogIdentity struct {
	tokenID   int
	tokenName string
	groupID   string
}

func setOperationalLogIdentity(c *gin.Context, tokenID int, tokenName, groupID string) {
	c.Set(operationalLogIdentityKey, operationalLogIdentity{
		tokenID:   tokenID,
		tokenName: tokenName,
		groupID:   groupID,
	})
}

func operationalLogIdentityFromContext(c *gin.Context) (operationalLogIdentity, bool) {
	value, ok := c.Get(operationalLogIdentityKey)
	if !ok {
		return operationalLogIdentity{}, false
	}

	identity, ok := value.(operationalLogIdentity)

	return identity, ok
}

var recordOperationalLog = model.RecordOperationalLog

func SetFailureStage(c *gin.Context, stage model.FailureStage, safeError string) {
	SetOperationalFailure(c, stage, "", safeError)
}

func SetOperationalFailure(
	c *gin.Context,
	stage model.FailureStage,
	errorCode string,
	safeError string,
) {
	c.Set(operationalFailureStageKey, stage)
	c.Set(operationalErrorCodeKey, errorCode)
	c.Set(operationalSafeErrorKey, safeError)
}

func MarkOperationalLogRecorded(c *gin.Context) {
	c.Set(operationalLogRecordedKey, true)
}

// ClearOperationalLogRecorded hands the request's log row back to the
// middleware, for example after the row a handler wrote was released.
func ClearOperationalLogRecorded(c *gin.Context) {
	c.Set(operationalLogRecordedKey, false)
}

func operationalLogRecorded(c *gin.Context) bool {
	return c.GetBool(operationalLogRecordedKey)
}

// SetCallBillingOperationID records the billing operation a request's
// rejection row carries: a handler calls it once money may be held for the
// request (key usage P2), for example after a native task's reservation.
func SetCallBillingOperationID(c *gin.Context, billingOperationID string) {
	c.Set(callBillingOperationKey, billingOperationID)
}

// GetCallBillingOperationID returns the operation set by
// SetCallBillingOperationID, or "".
func GetCallBillingOperationID(c *gin.Context) string {
	return c.GetString(callBillingOperationKey)
}

// operationalTokenName is the name of the key the request authenticated with,
// or of the key TokenAuth identified before rejecting it.
func operationalTokenName(c *gin.Context) string {
	if value, ok := c.Get(Token); ok {
		switch token := value.(type) {
		case model.TokenCache:
			return token.Name
		case *model.TokenCache:
			if token != nil {
				return token.Name
			}
		}
	}

	if identity, ok := operationalLogIdentityFromContext(c); ok {
		return identity.tokenName
	}

	return ""
}

// OperationalFieldsFromContext reads the request's log fields. The source comes
// from authenticated state only (model.UsageSource): the website Playground's
// hidden credential, or admin_demo for internal groups. The
// X-Token-Platform-Source header can no longer label another key's call as
// playground.
func OperationalFieldsFromContext(c *gin.Context) model.OperationalFields {
	stage, _ := c.Get(operationalFailureStageKey)
	failureStage, _ := stage.(model.FailureStage)

	requestSource := model.UsageSource(
		operationalTokenName(c),
		isInternalGroupContext(c),
		c.GetHeader(OperationalLogSourceHeader),
	)

	fields := model.BuildOperationalFields(
		requestSource,
		failureStage,
		c.GetString(operationalSafeErrorKey),
		c.GetString(operationalErrorCodeKey),
	)
	fields.RequestedModel = GetRequestedModel(c)
	fields.PublicModel = GetPublicModel(c)
	fields.PublicCapabilityModel = GetPublicCapabilityModel(c)
	fields.ResolvedCapability = GetResolvedCapability(c)

	return fields
}

func isInternalGroupContext(c *gin.Context) bool {
	value, ok := c.Get(Group)
	if !ok {
		return false
	}

	switch group := value.(type) {
	case model.GroupCache:
		return group.Status == model.GroupStatusInternal
	case *model.GroupCache:
		return group != nil && group.Status == model.GroupStatusInternal
	default:
		return false
	}
}

func OperationalLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if c.Writer.Status() < http.StatusBadRequest || operationalLogRecorded(c) {
			return
		}

		if err := recordRejectedGatewayLog(c); err != nil {
			common.GetLogger(c).Errorf("record rejected gateway log: %v", err)
		}
	}
}

func recordRejectedGatewayLog(c *gin.Context) error {
	now := time.Now()

	requestAt := GetRequestAt(c)
	if requestAt.IsZero() {
		requestAt = now
	}

	fields := OperationalFieldsFromContext(c)
	metadata := GetRequestMetadata(c)
	modelName, capability := model.PublicLogIdentity(
		GetRequestModel(c),
		fields.PublicModel,
		fields.ResolvedCapability,
	)

	relayMode := GetMode(c)
	if relayMode == mode.Unknown {
		if routeMode, ok := ReadRouteMode(c); ok {
			relayMode = routeMode
		}
	}

	entry := &model.Log{
		RequestID: model.EmptyNullString(GetRequestID(c)),
		RequestAt: requestAt,
		CreatedAt: now,
		Code:      c.Writer.Status(),
		Mode:      int(relayMode),
		IP:        model.EmptyNullString(maskOperationalIP(c.ClientIP())),
		ChannelID: GetChannelID(c),
		Endpoint: model.EmptyNullString(
			truncateOperationalText(c.Request.Method+" "+c.Request.URL.Path, 64),
		),
		Model:         modelName,
		Capability:    capability,
		User:          model.EmptyNullString(GetRequestUser(c)),
		RequestSource: fields.RequestSource,
		FailureStage:  fields.FailureStage,
		ErrorCode:     fields.ErrorCode,
		SafeError:     fields.SafeError,
		Metadata:      metadata,
		// Set only once money may be held for this request; NULL otherwise.
		BillingOperationID: model.EmptyNullString(GetCallBillingOperationID(c)),
	}

	identity, identified := operationalLogIdentityFromContext(c)

	if value, ok := c.Get(Token); ok {
		switch token := value.(type) {
		case model.TokenCache:
			entry.TokenID = token.ID
			entry.TokenName = token.Name
		case *model.TokenCache:
			entry.TokenID = token.ID
			entry.TokenName = token.Name
		}
	} else if identified {
		// A key that TokenAuth identified but rejected still owns the row, so
		// the rejection counts as that key's call.
		entry.TokenID = identity.tokenID
		entry.TokenName = identity.tokenName
	}

	if value, ok := c.Get(Group); ok {
		if group, ok := value.(model.GroupCache); ok {
			entry.GroupID = group.ID
		}
	} else if identified {
		entry.GroupID = identity.groupID
	}

	if value, ok := c.Get(customerValidationDetailKey); ok {
		if detail, ok := value.(*model.RequestDetail); ok {
			entry.RequestDetail = detail
		}
	}

	return recordOperationalLog(entry)
}

func truncateOperationalText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit])
}

func maskOperationalIP(value string) string {
	ip := net.ParseIP(value)
	if ip == nil {
		return ""
	}

	if ipv4 := ip.To4(); ipv4 != nil {
		return net.IPv4(ipv4[0], ipv4[1], ipv4[2], 0).String()
	}

	ipv6 := ip.To16()
	for index := 8; index < len(ipv6); index++ {
		ipv6[index] = 0
	}

	return ipv6.String()
}
