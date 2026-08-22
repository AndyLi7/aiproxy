package controller

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

const (
	defaultGroupVideoTaskPage     = 1
	defaultGroupVideoTaskPageSize = 20
)

// GetGroupVideoTasks godoc
//
//	@Summary		Get safe video task history for a group
//	@Description	Returns a paginated, customer-safe projection of asynchronous video tasks
//	@Tags			video tasks
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			group		path		string	true	"Group name"
//	@Param			page		query		int		false	"Page number"
//	@Param			per_page	query		int		false	"Items per page (1-100)"
//	@Success		200			{object}	middleware.APIResponse{data=model.GroupVideoTaskPage}
//	@Router			/api/video_tasks/{group} [get]
func GetGroupVideoTasks(c *gin.Context) {
	group := c.Param("group")
	if group == "" || len(group) > 64 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group")
		return
	}

	page, perPage, ok := groupVideoTaskPagination(c)
	if !ok {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid pagination")
		return
	}

	result, err := model.ListGroupVideoTasks(group, page, perPage)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, "failed to list video tasks")
		return
	}

	middleware.SuccessResponse(c, result)
}

func groupVideoTaskPagination(c *gin.Context) (page, perPage int, ok bool) {
	page = defaultGroupVideoTaskPage
	perPage = defaultGroupVideoTaskPageSize
	if raw := c.Query("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, false
		}
		page = parsed
	}
	if raw := c.Query("per_page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, false
		}
		perPage = parsed
	}
	if page <= 0 || page > model.MaxGroupVideoTaskPage ||
		perPage <= 0 || perPage > model.MaxGroupVideoTaskPageSize {
		return 0, 0, false
	}
	return page, perPage, true
}
