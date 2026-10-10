package controller

import (
	"bytes"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type gifTransport func(*http.Request) (*http.Response, error)

func (f gifTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGIFPublicSignedDownloadPreservesAnimationAndAuthorization(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "gif.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	previous := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = previous })
	t.Setenv("PUBLIC_IMAGE_BASE_URL", "https://gateway.test")
	expiry := time.Now().Add(30 * time.Minute)
	task := model.ImageTask{ID: "gif-task", Status: "completed", KeyFingerprint: strings.Repeat("a", 64), ArchiveRequired: true, ResultExpiresAt: &expiry, Data: []model.ImageOutput{{URL: "https://media.test/generated-results/images/gif-task/0.gif", Stored: true, ContentType: "image/gif"}}}
	require.NoError(t, db.Create(&task).Error)
	palette := color.Palette{color.Black, color.White}
	a := image.NewPaletted(image.Rect(0, 0, 2, 3), palette)
	b := image.NewPaletted(a.Rect, palette)
	b.SetColorIndex(0, 0, 1)
	var buf bytes.Buffer
	require.NoError(t, gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{a, b}, Delay: []int{7, 13}, LoopCount: 3}))
	payload := buf.Bytes()
	calls := 0
	client := func() *http.Client {
		return &http.Client{Transport: gifTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			require.Equal(t, task.Data[0].URL, r.URL.String())
			require.Empty(t, r.Header.Get("Authorization"))
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: http.Header{"Content-Type": []string{"image/gif"}}}, nil
		})}
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	public := publicImageTask(ctx, &task)
	require.Len(t, public.Data, 1)
	require.NotContains(t, public.Data[0].URL, "media.test")
	router := gin.New()
	router.GET("/v1/images/tasks/:id/content/:index", func(c *gin.Context) { getImageTaskContent(c, client) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", public.Data[0].URL, nil))
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "image/gif", rec.Header().Get("Content-Type"))
	require.Equal(t, payload, rec.Body.Bytes())
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	decoded, err := gif.DecodeAll(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	require.Len(t, decoded.Image, 2)
	require.Equal(t, []int{7, 13}, decoded.Delay)
	require.Equal(t, 3, decoded.LoopCount)
	// Public signed URLs share a fixed number of in-flight transfers.
	for range cap(imageContentSlots) {
		imageContentSlots <- struct{}{}
	}
	busy := httptest.NewRecorder()
	router.ServeHTTP(busy, httptest.NewRequest("GET", public.Data[0].URL, nil))
	for range cap(imageContentSlots) {
		<-imageContentSlots
	}
	require.Equal(t, 503, busy.Code)
	require.Equal(t, "1", busy.Header().Get("Retry-After"))
	require.Equal(t, 1, calls, "a refused transfer never fetches the stored image")
	for _, mutate := range []func(*url.URL){func(u *url.URL) { q := u.Query(); q.Set("signature", strings.Repeat("0", 64)); u.RawQuery = q.Encode() }, func(u *url.URL) { u.Path = strings.Replace(u.Path, "gif-task", "other-task", 1) }, func(u *url.URL) { u.Path = strings.TrimSuffix(u.Path, "0") + "1" }} {
		u, err := url.Parse(public.Data[0].URL)
		require.NoError(t, err)
		mutate(u)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest("GET", u.String(), nil))
		require.Equal(t, 404, rr.Code)
	}
	require.Equal(t, 1, calls)
	expired := time.Now().Add(-time.Second)
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).Update("result_expires_at", expired).Error)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", public.Data[0].URL, nil))
	require.Equal(t, 410, rec.Code)
	require.Equal(t, 1, calls)
}
