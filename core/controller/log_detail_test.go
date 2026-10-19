package controller_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestLogDetailOptionalBodyAndGroupIsolation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.RequestDetail{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	require.NoError(t, db.Create(&model.Log{ID: 1, GroupID: "owner"}).Error)

	router := gin.New()
	router.GET("/detail/:log_id", controller.GetLogDetail)
	router.GET("/group/:group/detail/:log_id", controller.GetGroupLogDetail)

	check := func(path string, status int) {
		t.Helper()

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		require.Equal(t, status, w.Code, w.Body.String())
	}
	check("/detail/1", 404)
	check("/group/owner/detail/1", 404)
	check("/detail/invalid", 400)
	check("/detail/0", 400)
	check("/group/owner/detail/-1", 400)
	require.NoError(t, db.Create(&model.RequestDetail{LogID: 1, ResponseBody: "saved"}).Error)
	check("/detail/1", 200)
	check("/group/owner/detail/1", 200)
	check("/group/other/detail/1", 404)
	require.NoError(t, db.Migrator().DropTable(&model.RequestDetail{}))
	check("/detail/1", 500)
}
