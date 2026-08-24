package middleware

import (
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
)

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

func GenRequestID(t time.Time) string {
	return strconv.FormatInt(t.UnixMicro(), 10)
}

const (
	RequestIDHeader = "X-Request-Id"
)

func SetRequestID(c *gin.Context, id string) {
	c.Set(RequestID, id)
	c.Header(RequestIDHeader, id)
	log := common.GetLogger(c)
	SetLogRequestIDField(log.Data, id)
}

func GetRequestID(c *gin.Context) string {
	return c.GetString(RequestID)
}

func RequestIDMiddleware(c *gin.Context) {
	now := GetRequestAt(c)
	id := c.GetHeader(RequestIDHeader)
	if !safeRequestID.MatchString(id) {
		id = GenRequestID(now)
	}
	SetRequestID(c, id)
}
