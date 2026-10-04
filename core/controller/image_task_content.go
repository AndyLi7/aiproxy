package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

// Output URLs are capability URLs, not upstream redirects. No provider URL or
// credential is embedded in them. A task's persisted, server-only key fingerprint
// makes signatures stable across restarts without introducing a new public secret.
func imageContentSignature(task *model.ImageTask, index int, expires int64, auxiliary ...string) string {
	mac := hmac.New(sha256.New, []byte(task.KeyFingerprint))
	if len(auxiliary) > 0 && auxiliary[0] != "" {
		fmt.Fprintf(mac, "image-auxiliary-content-v1\n%s\n%d\n%s\n%d", task.ID, index, auxiliary[0], expires)
	} else {
		fmt.Fprintf(mac, "image-content-v1\n%s\n%d\n%d", task.ID, index, expires)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func publicImageTask(c *gin.Context, task *model.ImageTask) *model.ImageTask {
	result := *task
	result.Phase = task.Status
	if task.Status == "result_processing" {
		result.Status = "in_progress"
	}
	if task.Status != "completed" {
		result.Data = nil
		result.ProviderMetadata = nil
	}
	group, exists := c.Get(middleware.Group)
	internal, ok := group.(model.GroupCache)
	if exists && ok && internal.Status == model.GroupStatusInternal {
		return &result
	}
	if result.Error != nil {
		result.Error = &model.ImageTaskError{Code: "generation_failed", Message: "Image generation failed. Contact support with the request ID."}
	}
	result.Data = model.CloneImageOutputs(task.Data)
	if task.Status != "completed" {
		result.Data = nil
		return &result
	}
	if task.ResultExpiresAt != nil && !time.Now().Before(*task.ResultExpiresAt) {
		result.Data = nil
		result.ResultAvailability = "expired"
		return &result
	}
	if task.ArchiveRequired {
		result.ResultAvailability = "stored"
	} else {
		result.ResultAvailability = "legacy"
	}
	expiry := time.Now().Add(time.Hour).UTC()
	if task.ResultExpiresAt != nil && task.ResultExpiresAt.Before(expiry) {
		expiry = *task.ResultExpiresAt
	}
	expires := expiry.Unix()
	for index := range result.Data {
		result.Data[index].Stored = false
		result.Data[index].URLExpiresAt = &expiry
		// Historical tasks without a signing key must not fall back to upstream URLs.
		if len(task.KeyFingerprint) != 64 || !model.ValidAuxiliaryImages(result.Data[index]) {
			result.Data = nil
			result.Status = "failed"
			result.Error = &model.ImageTaskError{Code: "result_unavailable", Message: "The image result is unavailable."}
			break
		}
		origin, err := url.Parse(os.Getenv("PUBLIC_IMAGE_BASE_URL"))
		if err != nil || origin == nil || (origin.Scheme != "https" && !(origin.Scheme == "http" && (origin.Hostname() == "127.0.0.1" || origin.Hostname() == "localhost" || origin.Hostname() == "::1"))) || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
			result.Data = nil
			result.Status = "failed"
			result.Error = &model.ImageTaskError{Code: "result_unavailable", Message: "The image result is unavailable."}
			break
		}
		sign := func(asset *model.ImageOutput, name string) {
			target := url.URL{Scheme: origin.Scheme, Host: origin.Host, Path: fmt.Sprintf("/v1/images/tasks/%s/content/%d", task.ID, index)}
			query := target.Query()
			query.Set("expires", strconv.FormatInt(expires, 10))
			query.Set("signature", imageContentSignature(task, index, expires, name))
			if name != "" {
				query.Set("auxiliary", name)
			}
			target.RawQuery = query.Encode()
			asset.URL = target.String()
			asset.Stored = false
			asset.URLExpiresAt = &expiry
		}
		sign(&result.Data[index], "")
		for name, asset := range result.Data[index].AuxiliaryImages {
			if asset != nil {
				sign(asset, name)
			}
		}
	}
	return &result
}

func validImageContentSignature(task *model.ImageTask, index int, expires int64, signature string, now int64, auxiliary ...string) bool {
	if len(task.KeyFingerprint) != 64 || index < 0 || index >= len(task.Data) || expires < now || expires > now+3600 {
		return false
	}
	name := ""
	if len(auxiliary) > 0 {
		name = auxiliary[0]
	}
	if imageContentAsset(task, index, name) == nil {
		return false
	}
	decoded, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	expected, _ := hex.DecodeString(imageContentSignature(task, index, expires, name))
	return hmac.Equal(decoded, expected)
}

func imageContentAsset(task *model.ImageTask, index int, name string) *model.ImageOutput {
	if index < 0 || index >= len(task.Data) || !model.ValidAuxiliaryImages(task.Data[index]) {
		return nil
	}
	if name == "" {
		return &task.Data[index]
	}
	if !model.ValidAuxiliaryImageName(name) {
		return nil
	}
	return task.Data[index].AuxiliaryImages[name]
}

func publicImageIP(ip net.IP) bool {
	for _, block := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24"} {
		_, network, _ := net.ParseCIDR(block)
		if network.Contains(ip) {
			return false
		}
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

// Resolve and validate at dial time (not a separate preflight) to avoid DNS
// rebinding. Do not use ambient HTTP proxies or follow upstream redirects.
func imageContentClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 1 << 20,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if port != "443" {
				return nil, fmt.Errorf("invalid image port")
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no public address")
			}
			for _, ip := range ips {
				if !publicImageIP(ip.IP) {
					return nil, fmt.Errorf("non-public address")
				}
			}
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}}
}

func GetImageTaskContent(c *gin.Context) {
	getImageTaskContent(c, imageContentClient)
}

// Signed result URLs are public and each transfer buffers up to 20 MiB, so
// only a fixed number may be in flight at once.
var imageContentSlots = make(chan struct{}, 32)

func getImageTaskContent(c *gin.Context, newClient func() *http.Client) {
	fail := func(status int) {
		c.Header("Cache-Control", "no-store")
		c.JSON(status, gin.H{"error": gin.H{"code": "result_unavailable", "message": "The image result is unavailable."}})
	}
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || len(c.Query("signature")) != 64 || !imageRequestID.MatchString(c.Param("id")) {
		fail(404)
		return
	}
	expires, err := strconv.ParseInt(c.Query("expires"), 10, 64)
	if err != nil {
		fail(404)
		return
	}
	var task model.ImageTask
	if model.LogDB.Where("id = ?", c.Param("id")).First(&task).Error != nil || !validImageContentSignature(&task, index, expires, c.Query("signature"), time.Now().Unix(), c.Query("auxiliary")) || task.Status != "completed" {
		fail(404)
		return
	}
	if task.ResultExpiresAt != nil && !time.Now().Before(*task.ResultExpiresAt) {
		fail(410)
		return
	}
	target, err := url.Parse(imageContentAsset(&task, index, c.Query("auxiliary")).URL)
	if err != nil || target.Scheme != "https" || target.User != nil || target.Hostname() == "" {
		fail(502)
		return
	}
	select {
	case imageContentSlots <- struct{}{}:
		defer func() { <-imageContentSlots }()
	default:
		c.Header("Retry-After", "1")
		fail(503)
		return
	}
	// A slow reader must not hold a slot indefinitely. The server has no write
	// timeout, so the deadline is cleared again for keep-alive reuse.
	controller := http.NewResponseController(c.Writer)
	_ = controller.SetWriteDeadline(time.Now().Add(time.Minute))
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		fail(502)
		return
	}
	client := newClient()
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		fail(502)
		return
	}
	defer response.Body.Close()
	const limit = 20 * 1024 * 1024
	if response.StatusCode != 200 || response.ContentLength > limit {
		fail(502)
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		fail(502)
		return
	}
	contentType := http.DetectContentType(body)
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "image/svg+xml") {
		contentType = "image/svg+xml"
		c.Header("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'")
	}
	if contentType != "image/svg+xml" && contentType != "image/png" && contentType != "image/jpeg" && contentType != "image/webp" && contentType != "image/gif" {
		fail(502)
		return
	}
	c.Header("Cache-Control", "private, max-age=60")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Data(200, contentType, body)
}
