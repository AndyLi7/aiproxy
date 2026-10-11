package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

const (
	maxLastAPIRequestGroups     = 100
	maxLastAPIRequestGroupRunes = 64
)

// lastAPIRequestLookupBudget bounds the whole lookup below the app's 5s client
// timeout, so the groups that finished are still returned when others are slow.
var lastAPIRequestLookupBudget = 4 * time.Second

type LastAPIRequestRequest struct {
	Groups []string `json:"groups"`
}

type LastAPIRequestItem struct {
	Group string `json:"group"`
	// LastRequestAt is the log row's created_at in Unix milliseconds.
	LastRequestAt int64 `json:"last_request_at"`
}

type LastAPIRequestResponse struct {
	Items []LastAPIRequestItem `json:"items"`
	// UncheckedGroups lists requested groups that were not looked up because the
	// time budget ran out. Their absence from Items means "unknown", not "none".
	UncheckedGroups []string `json:"unchecked_groups,omitempty"`
}

func validateLastAPIRequestGroups(groups []string) error {
	if len(groups) == 0 {
		return errors.New("groups is required")
	}

	if len(groups) > maxLastAPIRequestGroups {
		return fmt.Errorf("at most %d groups are allowed", maxLastAPIRequestGroups)
	}

	seen := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if strings.TrimSpace(group) == "" {
			return errors.New("group must not be empty")
		}

		if utf8.RuneCountInString(group) > maxLastAPIRequestGroupRunes {
			return fmt.Errorf("group must be at most %d characters", maxLastAPIRequestGroupRunes)
		}

		if _, ok := seen[group]; ok {
			return errors.New("groups must be distinct")
		}

		seen[group] = struct{}{}
	}

	return nil
}

// GetGroupsLastAPIRequest godoc
//
//	@Summary		Batch get the last API request time of groups
//	@Description	For each group, returns the created_at (Unix ms) of its newest request log
//	@Description	whose request_source is not playground or admin_demo; rows with no source
//	@Description	(legacy) count. Groups without such a row are omitted. Groups not looked up
//	@Description	within the time budget are listed in unchecked_groups.
//	@Tags			logs
//	@Accept			json
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			body	body		controller.LastAPIRequestRequest	true	"1-100 distinct group IDs"
//	@Success		200		{object}	middleware.APIResponse{data=controller.LastAPIRequestResponse}
//	@Router			/api/logs/last_api_request [post]
func GetGroupsLastAPIRequest(c *gin.Context) {
	var req LastAPIRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := validateLastAPIRequestGroups(req.Groups); err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), lastAPIRequestLookupBudget)
	defer cancel()

	found, unchecked, err := model.GetGroupsLastAPIRequestAt(ctx, req.Groups)
	if err != nil {
		if len(unchecked) == 0 {
			common.GetLogger(c).Errorf("last api request lookup failed: %v", err)
			middleware.ErrorResponse(
				c,
				http.StatusInternalServerError,
				"unable to read last api requests",
			)

			return
		}

		common.GetLogger(c).Warnf(
			"last api request lookup stopped early: %v (%d of %d groups unchecked)",
			err, len(unchecked), len(req.Groups),
		)
	}

	items := make([]LastAPIRequestItem, 0, len(found))
	for _, group := range req.Groups {
		if at, ok := found[group]; ok {
			items = append(items, LastAPIRequestItem{
				Group:         group,
				LastRequestAt: at.UnixMilli(),
			})
		}
	}

	middleware.SuccessResponse(c, LastAPIRequestResponse{
		Items:           items,
		UncheckedGroups: unchecked,
	})
}
