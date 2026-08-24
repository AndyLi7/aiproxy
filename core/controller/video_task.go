package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

const maxVideoTaskPage = 10000

func GetGroupVideoTasks(c *gin.Context) {
	group := c.Param("group")
	page, pageErr := strconv.Atoi(c.Query("page"))
	perPage, perPageErr := strconv.Atoi(c.Query("per_page"))
	if !validVideoTaskGroup(group) || pageErr != nil || perPageErr != nil ||
		page < 1 || page > maxVideoTaskPage || perPage < 1 || perPage > 100 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid video task query")
		return
	}

	result, err := model.ListGroupVideoTasks(group, page, perPage)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, "video task history unavailable")
		return
	}

	middleware.SuccessResponse(c, result)
}

func validVideoTaskGroup(group string) bool {
	if group == "" || len(group) > 64 || group != strings.TrimSpace(group) {
		return false
	}
	for _, character := range group {
		if character <= 0x1f || character == 0x7f {
			return false
		}
	}
	return true
}
