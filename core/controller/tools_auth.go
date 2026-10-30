package controller

// RECONSTRUCTED from the application's signToolsExecutionRequest contract.
import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm/clause"
)

const toolsBodyLimit = 4 << 20
const toolsClaimsKey = "reconstructed-tools-claims"

type toolsClaims struct {
	V         int    `json:"v"`
	UserID    string `json:"userId"`
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	BodyHash  string `json:"bodySha256"`
	Revision  string `json:"revision"`
	IAT       int64  `json:"iat"`
	Nonce     string `json:"nonce"`
}

var toolsNoncePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var toolsIDPattern = regexp.MustCompile(`^tools_[A-Za-z0-9_-]{1,180}$`)
var toolsHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func toolsDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func toolsText(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func toolsError(c *gin.Context, status int, code string) {
	nativetask.WriteError(c.Writer, status, code)
	c.Abort()
}
func ToolsAuth(c *gin.Context) {
	key := os.Getenv("TOOLS_INTERNAL_KEY")
	if len(key) < 32 || model.DB == nil {
		toolsError(c, 503, "tools_authentication_unavailable")
		return
	}
	header := c.GetHeader("X-Tools-Authorization")
	if len(header) > 8192 {
		toolsError(c, 401, "tools_authentication_failed")
		return
	}
	parts := strings.Split(header, ".")
	if len(parts) != 2 {
		toolsError(c, 401, "tools_authentication_failed")
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		toolsError(c, 401, "tools_authentication_failed")
		return
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("musespan-tools-execution-v1." + parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		toolsError(c, 401, "tools_authentication_failed")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	var claims toolsClaims
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err != nil || dec.Decode(&claims) != nil || dec.Decode(new(any)) != io.EOF {
		toolsError(c, 401, "tools_authentication_failed")
		return
	}
	now := time.Now().Unix()
	if claims.V != 1 || !toolsText(claims.UserID, 200) || !toolsText(claims.AgentID, 200) || !toolsText(claims.SessionID, 200) || !toolsText(claims.RequestID, 200) || !toolsNoncePattern.MatchString(claims.Nonce) || claims.IAT < now-300 || claims.IAT > now+30 || claims.Method != c.Request.Method || claims.Path != c.Request.URL.RequestURI() || claims.RequestID != c.GetHeader("X-Request-Id") || claims.Revision != c.GetHeader("X-Tools-Execution-Revision") || (claims.Revision != "" && !toolsHashPattern.MatchString(claims.Revision)) {
		toolsError(c, 401, "tools_authentication_failed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, toolsBodyLimit))
	if err != nil {
		toolsError(c, 413, "tools_media_request_too_large")
		return
	}
	if !hmac.Equal([]byte(claims.BodyHash), []byte(toolsDigest(body))) {
		toolsError(c, 401, "tools_authentication_failed")
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	// Persistent nonce uniqueness also protects restarts and concurrent processes.
	row := model.ToolsRequestNonce{Nonce: claims.Nonce, ExpiresAt: claims.IAT + 301}
	result := model.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil {
		toolsError(c, 503, "tools_authentication_unavailable")
		return
	}
	if result.RowsAffected != 1 {
		toolsError(c, 401, "tools_request_replayed")
		return
	}
	// Only entries that cannot pass the timestamp gate can be removed.
	model.DB.Where("expires_at < ?", now-30).Delete(&model.ToolsRequestNonce{})
	c.Set(toolsClaimsKey, claims)
	c.Next()
}
func toolsActor(c *gin.Context) toolsClaims { return c.MustGet(toolsClaimsKey).(toolsClaims) }
