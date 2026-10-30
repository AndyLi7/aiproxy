package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

const AgentExecutionConfigKey = model.AgentExecutionKey

// AgentAccess is a private product authorization view. It does not publish a
// model into customer discovery. Group sets and token model limits still apply.
// The descriptor contains no channel key, URL override or credential fingerprint.
func AgentAccess(c *gin.Context) {
	token := middleware.GetToken(c)
	if !model.IsAgentCredentialName(token.Name) {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	name := c.Query("model")
	if len(name) == 0 || len(name) > 191 || token.FindModel(name) == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	value, _ := c.Get(middleware.ModelCaches)

	caches, ok := value.(*model.ModelCaches)
	if !ok || caches == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	mc, ok := caches.EnabledModelConfigsMap[name]
	if !ok || mc.Config[AgentExecutionConfigKey] == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	mc = middleware.GetGroupAdjustedModelConfig(middleware.GetGroup(c), mc)
	// Descriptor validation belongs to the same closed schema used by app tools.
	// Admin-only configuration is the authority; ordinary API keys cannot read it.
	encoded, err := json.Marshal(mc.Price)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	var price map[string]any
	if json.Unmarshal(encoded, &price) != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	currency, version, validPrice := mc.RetailPricingMetadata()
	if validPrice {
		price["currency"], price["pricing_version"] = currency, version
	}

	response := gin.H{
		"model":    name,
		"agent":    mc.Config[AgentExecutionConfigKey],
		"price":    price,
		"revision": agentExecutionRevision(mc),
	}
	if binding, valid := parseNativeRouteBinding(mc); valid {
		response["contract"] = binding.Contract
	}

	c.JSON(http.StatusOK, response)
}

// Hash includes the full native binding/quote and effective group prices. Only
// the hash leaves the gateway; channel fingerprints never enter the app DTO.
func agentExecutionRevision(mc model.ModelConfig) string {
	raw, err := json.Marshal(
		map[string]any{
			"model":  mc.Model,
			"agent":  mc.Config[AgentExecutionConfigKey],
			"native": mc.Config[NativeResultConfigKey],
			"config": mc.Config,
			"price":  mc.Price,
		},
	)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:])
}
