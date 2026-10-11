//nolint:testpackage
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
)

func openLastAPIRequestControllerDB(t *testing.T) *gorm.DB {
	t.Helper()

	database, err := model.OpenSQLite(
		filepath.Join(t.TempDir(), "controller-last-api-request.db"),
	)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	previousLogDB := model.LogDB
	model.LogDB = database

	t.Cleanup(func() {
		model.LogDB = previousLogDB

		sqlDB, sqlErr := database.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	if err := database.AutoMigrate(&model.Log{}, &model.RequestDetail{}); err != nil {
		t.Fatalf("migrate log database: %v", err)
	}

	return database
}

func serveLastAPIRequest(ctx context.Context, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.POST("/api/logs/last_api_request", GetGroupsLastAPIRequest)

	request := httptest.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"/api/logs/last_api_request",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	return recorder
}

type lastAPIRequestPayload struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Items []struct {
			Group         string `json:"group"`
			LastRequestAt int64  `json:"last_request_at"`
		} `json:"items"`
		UncheckedGroups []string `json:"unchecked_groups"`
	} `json:"data"`
}

func decodeLastAPIRequest(
	t *testing.T,
	recorder *httptest.ResponseRecorder,
) lastAPIRequestPayload {
	t.Helper()

	var payload lastAPIRequestPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}

	return payload
}

func groupsBody(t *testing.T, groups ...string) string {
	t.Helper()

	raw, err := json.Marshal(LastAPIRequestRequest{Groups: groups})
	if err != nil {
		t.Fatalf("encode groups: %v", err)
	}

	return string(raw)
}

func numberedGroups(count int) []string {
	groups := make([]string, 0, count)
	for i := range count {
		groups = append(groups, "u_"+strconv.Itoa(i))
	}

	return groups
}

func apiLog(group, tokenName, source string, code int, createdAt time.Time) model.Log {
	return model.Log{
		GroupID:       group,
		TokenName:     tokenName,
		RequestSource: source,
		Code:          code,
		CreatedAt:     createdAt,
	}
}

func TestGetGroupsLastAPIRequestRejectsInvalidBodies(t *testing.T) {
	openLastAPIRequestControllerDB(t)

	tooLong := strings.Repeat("g", maxLastAPIRequestGroupRunes+1)

	tests := map[string]string{
		"empty body":          "",
		"malformed json":      `{"groups":`,
		"groups missing":      `{}`,
		"groups null":         `{"groups":null}`,
		"groups not an array": `{"groups":"u_a"}`,
		"group not a string":  `{"groups":[1]}`,
		"no groups":           groupsBody(t),
		"too many groups":     groupsBody(t, numberedGroups(maxLastAPIRequestGroups+1)...),
		"empty group":         groupsBody(t, "u_a", ""),
		"blank group":         groupsBody(t, "   "),
		"group too long":      groupsBody(t, tooLong),
		"duplicate groups":    groupsBody(t, "u_a", "u_b", "u_a"),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := serveLastAPIRequest(t.Context(), body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}

			payload := decodeLastAPIRequest(t, recorder)
			if payload.Success || payload.Message == "" {
				t.Fatalf("response = %s, want an error message", recorder.Body.String())
			}
		})
	}
}

func TestGetGroupsLastAPIRequestAcceptsBoundaryLimits(t *testing.T) {
	openLastAPIRequestControllerDB(t)

	tests := map[string]string{
		"100 groups":      groupsBody(t, numberedGroups(maxLastAPIRequestGroups)...),
		"64 char group":   groupsBody(t, strings.Repeat("g", maxLastAPIRequestGroupRunes)),
		"64 rune group":   groupsBody(t, strings.Repeat("组", maxLastAPIRequestGroupRunes)),
		"unknown field":   `{"groups":["u_a"],"exclude_token_names":["playground"]}`,
		"single group id": groupsBody(t, "u_a"),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := serveLastAPIRequest(t.Context(), body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestGetGroupsLastAPIRequestReturnsUnixMillisInRequestOrder(t *testing.T) {
	database := openLastAPIRequestControllerDB(t)
	base := time.Unix(1_790_000_000, 0)

	logs := []model.Log{
		apiLog("u_a", "key-a", model.RequestSourceAPI, 200, base),
		apiLog("u_a", "playground", model.RequestSourcePlayground, 200, base.Add(time.Hour)),
		apiLog("u_b", "playground", model.RequestSourceAPI, 202, base.Add(2*time.Second)),
		apiLog("u_demo", "key-a", model.RequestSourceAdminDemo, 200, base),
	}
	if err := database.Create(&logs).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	recorder := serveLastAPIRequest(
		t.Context(),
		groupsBody(t, "u_b", "u_none", "u_demo", "u_a"),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	if strings.Contains(recorder.Body.String(), "unchecked_groups") {
		t.Fatalf("body = %s, want no unchecked_groups", recorder.Body.String())
	}

	payload := decodeLastAPIRequest(t, recorder)
	if !payload.Success || len(payload.Data.Items) != 2 {
		t.Fatalf("response = %s, want two items", recorder.Body.String())
	}

	wantB := base.Add(2 * time.Second).UnixMilli()
	if got := payload.Data.Items[0]; got.Group != "u_b" || got.LastRequestAt != wantB {
		t.Fatalf("first item = %+v, want u_b at %d", got, wantB)
	}

	wantA := base.UnixMilli()
	if got := payload.Data.Items[1]; got.Group != "u_a" || got.LastRequestAt != wantA {
		t.Fatalf("second item = %+v, want u_a at %d", got, wantA)
	}
}

func TestGetGroupsLastAPIRequestReturnsEmptyItemsArray(t *testing.T) {
	openLastAPIRequestControllerDB(t)

	recorder := serveLastAPIRequest(t.Context(), groupsBody(t, "u_none"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	if !strings.Contains(recorder.Body.String(), `"items":[]`) {
		t.Fatalf("body = %s, want an empty items array", recorder.Body.String())
	}
}

func TestGetGroupsLastAPIRequestListsGroupsLeftUncheckedByTheBudget(t *testing.T) {
	database := openLastAPIRequestControllerDB(t)

	seeded := apiLog("u_a", "key-a", model.RequestSourceAPI, 200, time.Unix(1_790_000_000, 0))
	if err := database.Create(&seeded).Error; err != nil {
		t.Fatalf("seed log: %v", err)
	}

	previousBudget := lastAPIRequestLookupBudget
	lastAPIRequestLookupBudget = 0

	t.Cleanup(func() { lastAPIRequestLookupBudget = previousBudget })

	recorder := serveLastAPIRequest(t.Context(), groupsBody(t, "u_a", "u_b"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	payload := decodeLastAPIRequest(t, recorder)
	if len(payload.Data.Items) != 0 ||
		strings.Join(payload.Data.UncheckedGroups, ",") != "u_a,u_b" {
		t.Fatalf("response = %s, want no items and both groups unchecked", recorder.Body.String())
	}
}

func TestGetGroupsLastAPIRequestReturnsGroupsThatFinishedBeforeCancellation(t *testing.T) {
	database := openLastAPIRequestControllerDB(t)
	base := time.Unix(1_790_000_000, 0)

	logs := []model.Log{
		apiLog("u_a", "key-a", model.RequestSourceAPI, 200, base),
		apiLog("u_b", "key-a", model.RequestSourceAPI, 200, base),
	}
	if err := database.Create(&logs).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	// Cancel the request as soon as the first group's query has finished.
	if err := database.Callback().Query().After("gorm:query").
		Register("test:cancel_after_first_query", func(*gorm.DB) { cancel() }); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	recorder := serveLastAPIRequest(ctx, groupsBody(t, "u_a", "u_b"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	payload := decodeLastAPIRequest(t, recorder)
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].Group != "u_a" ||
		strings.Join(payload.Data.UncheckedGroups, ",") != "u_b" {
		t.Fatalf("response = %s, want u_a answered and u_b unchecked", recorder.Body.String())
	}
}

func TestGetGroupsLastAPIRequestHidesDatabaseErrors(t *testing.T) {
	database := openLastAPIRequestControllerDB(t)

	if err := database.Migrator().DropTable(&model.RequestDetail{}, &model.Log{}); err != nil {
		t.Fatalf("drop logs: %v", err)
	}

	recorder := serveLastAPIRequest(t.Context(), groupsBody(t, "u_a"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", recorder.Code, recorder.Body.String())
	}

	payload := decodeLastAPIRequest(t, recorder)
	if payload.Message != "unable to read last api requests" {
		t.Fatalf("message = %q, want the generic message", payload.Message)
	}
}
