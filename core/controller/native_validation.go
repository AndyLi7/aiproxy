package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/middleware"
)

// ValidateNativeContract is admin-only, pure compilation. It never reserves
// funds, accesses task storage, routes a request or contacts a provider.
func ValidateNativeContract(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, nativeresult.MaxBytes+1))
	if err != nil || len(raw) > nativeresult.MaxBytes {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid native contract")
		return
	}
	if _, err = nativeresult.CompileTaskContract(raw); err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, "native contract compilation failed")
		return
	}
	digest := sha256.Sum256(raw)
	middleware.SuccessResponse(c, gin.H{"schemaCompiled": true, "validatorVersion": 1, "contractDigest": "sha256:" + hex.EncodeToString(digest[:])})
}
