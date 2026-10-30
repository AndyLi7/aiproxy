package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
)

const nativeImageURLLifetime = 10 * time.Minute

// Use the existing private application/gateway storage transport secret with
// domain separation. KeyFingerprint is a provider-key digest, not a signing
// secret: config exports may contain that digest, so it is deliberately unused.
func nativeImageSignature(
	task *model.NativeTask,
	receipt ownedartifact.Receipt,
	expires int64,
) (string, bool) {
	secret := os.Getenv("EXTERNAL_BALANCE_KEY")
	if len(secret) < 32 {
		return "", false
	}

	raw, err := json.Marshal(
		[]any{"native-image-content-v1", task.ID, task.GroupID, task.TokenID, 0, expires, receipt},
	)
	if err != nil {
		return "", false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)

	return hex.EncodeToString(mac.Sum(nil)), true
}

// Only an owner-checked request may mint a capability URL. The token contains
// no customer key, provider credential, storage key or upstream address.
func signedNativeImageURL(
	task *model.NativeTask,
	group string,
	token int,
	now time.Time,
) (string, bool) {
	if task == nil || task.GroupID != group || task.TokenID != token {
		return "", false
	}

	receipt, ok := nativetask.SingleOwnedImage(task)
	if !ok {
		return "", false
	}

	origin, ok := publicImageOrigin()
	if !ok {
		return "", false
	}

	expires := now.Add(nativeImageURLLifetime).Unix()

	signature, ok := nativeImageSignature(task, receipt, expires)
	if !ok {
		return "", false
	}

	target := url.URL{
		Scheme: origin.Scheme,
		Host:   origin.Host,
		Path:   "/v1/images/native-results/" + task.ID + "/0",
	}
	values := target.Query()
	values.Set("expires", strconv.FormatInt(expires, 10))
	values.Set("signature", signature)
	target.RawQuery = values.Encode()

	return target.String(), true
}

func validNativeImageSignature(
	task *model.NativeTask,
	index string,
	expires int64,
	signature string,
	now time.Time,
) bool {
	if index != "0" || expires <= now.Unix() || expires > now.Add(nativeImageURLLifetime).Unix() {
		return false
	}

	receipt, ok := nativetask.SingleOwnedImage(task)
	if !ok {
		return false
	}

	expected, ok := nativeImageSignature(task, receipt, expires)
	if !ok {
		return false
	}

	decoded, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}

	expectedBytes, _ := hex.DecodeString(expected)

	return hmac.Equal(decoded, expectedBytes)
}

// This separate signed route does not relax /model-tasks/:id/artifacts/:index.
func GetNativeImageContent(c *gin.Context) {
	getNativeImageContent(c, &nativetask.HTTP{})
}

func getNativeImageContent(c *gin.Context, h *nativetask.HTTP) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Referrer-Policy", "no-referrer")

	fail := func() {
		c.AbortWithStatusJSON(
			404,
			gin.H{
				"error": gin.H{
					"code":    "image_not_found",
					"message": "Image download is unavailable.",
				},
			},
		)
	}
	query := c.Request.URL.Query()

	id := c.Param("id")
	if model.LogDB == nil || len(id) != 67 || !strings.HasPrefix(id, "oi_") || len(query) != 2 ||
		len(query["expires"]) != 1 ||
		len(query["signature"]) != 1 {
		fail()
		return
	}

	expires, err := strconv.ParseInt(query.Get("expires"), 10, 64)
	if err != nil {
		fail()
		return
	}

	var task model.NativeTask
	if model.LogDB.Where("id = ?", id).First(&task).Error != nil ||
		!validNativeImageSignature(
			&task,
			c.Param("index"),
			expires,
			query.Get("signature"),
			time.Now(),
		) {
		fail()
		return
	}

	receipt, ok := nativetask.SingleOwnedImage(&task)
	if !ok {
		fail()
		return
	}
	// Signature proves a previously owner-authorized capability. Reuse native
	// receipt verification, range handling, per-owner slots and transfer deadlines.
	h.ServeOwnedArtifact(c.Writer, c.Request, task.GroupID, task.TokenID, task.ID, 0, receipt)
}
