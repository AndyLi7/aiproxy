package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"io"
	"net/http"
	"strconv"
)

func routingChannelID(c *gin.Context) (int, bool) {
	id, e := strconv.Atoi(c.Param("id"))
	if e != nil || id <= 0 || strconv.Itoa(id) != c.Param("id") {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_channel_routing_request")
		return 0, false
	}
	return id, true
}
func GetChannelRouting(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}
	id, ok := routingChannelID(c)
	if !ok {
		return
	}
	request := c.Param("requestId")
	if request != "" && !model.ValidMigrationReadOperationID(request) {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_channel_routing_request")
		return
	}
	middleware.SuccessResponse(c, model.ReadChannelRouting(c.Request.Context(), id, request))
}
func PatchChannelRouting(c *gin.Context) {
	id, ok := routingChannelID(c)
	if !ok {
		return
	}
	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, model.ChannelRoutingBodyLimit+1))
	if e != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_channel_routing_request")
		return
	}
	q, e := model.ParseChannelRoutingRequest(raw)
	if e != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_channel_routing_request")
		return
	}
	middleware.SuccessResponse(c, model.PatchChannelRouting(c.Request.Context(), id, q))
}
