package middleware

import (
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
)

const OperationalLogSourceHeader = "X-Token-Platform-Source"

const (
	operationalFailureStageKey = "operational_failure_stage"
	operationalSafeErrorKey    = "operational_safe_error"
	operationalLogRecordedKey  = "operational_log_recorded"
)

var recordOperationalLog = model.RecordOperationalLog

func SetFailureStage(c *gin.Context, stage model.FailureStage, safeError string) {
	c.Set(operationalFailureStageKey, stage)
	c.Set(operationalSafeErrorKey, safeError)
}

func MarkOperationalLogRecorded(c *gin.Context) {
	c.Set(operationalLogRecordedKey, true)
}

func operationalLogRecorded(c *gin.Context) bool {
	return c.GetBool(operationalLogRecordedKey)
}

func OperationalFieldsFromContext(c *gin.Context) model.OperationalFields {
	stage, _ := c.Get(operationalFailureStageKey)
	failureStage, _ := stage.(model.FailureStage)
	return model.BuildOperationalFields(
		c.GetHeader(OperationalLogSourceHeader),
		failureStage,
		c.GetString(operationalSafeErrorKey),
	)
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

	entry := &model.Log{
		RequestID:     model.EmptyNullString(GetRequestID(c)),
		RequestAt:     requestAt,
		CreatedAt:     now,
		Code:          c.Writer.Status(),
		Mode:          int(GetMode(c)),
		IP:            model.EmptyNullString(maskOperationalIP(c.ClientIP())),
		ChannelID:     GetChannelID(c),
		Endpoint:      model.EmptyNullString(truncateOperationalText(c.Request.Method+" "+c.Request.URL.Path, 64)),
		Model:         GetRequestModel(c),
		User:          model.EmptyNullString(GetRequestUser(c)),
		RequestSource: fields.RequestSource,
		FailureStage:  fields.FailureStage,
		SafeError:     fields.SafeError,
	}

	if value, ok := c.Get(Token); ok {
		switch token := value.(type) {
		case model.TokenCache:
			entry.TokenID = token.ID
			entry.TokenName = token.Name
		case *model.TokenCache:
			entry.TokenID = token.ID
			entry.TokenName = token.Name
		}
	}
	if value, ok := c.Get(Group); ok {
		if group, ok := value.(model.GroupCache); ok {
			entry.GroupID = group.ID
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
