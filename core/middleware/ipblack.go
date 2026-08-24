package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/ipblack"
	"github.com/labring/aiproxy/core/model"
)

func IPBlock(c *gin.Context) {
	ip := c.ClientIP()

	isBlock := ipblack.GetIPIsBlockAnyWay(c.Request.Context(), ip)
	if isBlock {
		AbortOperationally(c, model.FailureStageRateLimit, http.StatusForbidden, "please try again later")
		c.Abort()
		return
	}

	c.Next()
}
