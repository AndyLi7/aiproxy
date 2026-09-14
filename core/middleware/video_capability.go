package middleware

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic/ast"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

var supportedVideoCapabilities = []string{
	string(model.ModelCapabilityTextToVideo),
	string(model.ModelCapabilityImageToVideo),
}

func resolveVideoCapability(
	c *gin.Context,
	requestMode mode.Mode,
	requestModel string,
) (string, string, error) {
	if publicModel, capability, ok := model.ParseModelCapabilityKey(requestModel); ok {
		if requestMode == mode.Videos {
			return "", "", &publicVideoRequestValidationError{
				code:     "invalid_parameter",
				message:  "model must be a public model ID",
				param:    "model",
				expected: "public model ID",
			}
		}
		setVideoCapabilityContext(c, publicModel, requestModel, capability)
		return publicModel, requestModel, nil
	}

	publicModel := requestModel
	if requestMode != mode.Videos {
		setVideoCapabilityContext(c, publicModel, requestModel, "")
		return publicModel, requestModel, nil
	}
	// Public catalog IDs carry the capability as their last path component.
	// Keep the internal route separate and preserve legacy model + capability callers.
	if slash := strings.LastIndex(requestModel, "/"); slash > 0 {
		embedded := model.ModelCapability(requestModel[slash+1:])
		if embedded.Valid() {
			explicit, err := getInitialVideoCapability(c)
			if err != nil {
				validation, missing := err.(*publicVideoRequestValidationError)
				if !missing || validation.code != "missing_capability" {
					return "", "", err
				}
			} else if explicit != embedded {
				return "", "", &publicVideoRequestValidationError{code: "invalid_parameter", message: "capability does not match model ID", param: "capability"}
			}
			publicModel = requestModel[:slash]
			route, err := model.BuildModelCapabilityKey(publicModel, embedded)
			if err != nil {
				return "", "", &publicVideoRequestValidationError{code: "invalid_parameter", message: "invalid public model ID", param: "model"}
			}
			setVideoCapabilityContext(c, publicModel, route, embedded)
			return publicModel, route, nil
		}
	}

	capability, err := getInitialVideoCapability(c)
	if err != nil {
		return "", "", err
	}

	routingModel, err := model.BuildModelCapabilityKey(publicModel, capability)
	if err != nil {
		return "", "", &publicVideoRequestValidationError{
			code:          "unsupported_capability",
			message:       fmt.Sprintf("unsupported video capability `%s`", capability),
			param:         "capability",
			value:         string(capability),
			allowedValues: supportedVideoCapabilities,
		}
	}

	setVideoCapabilityContext(c, publicModel, routingModel, capability)
	return publicModel, routingModel, nil
}

func getInitialVideoCapability(c *gin.Context) (model.ModelCapability, error) {
	var rawCapability string
	var err error
	if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
		rawCapability, err = getLimitedMultipartFormValue(c.Request, "capability")
	} else {
		var node *ast.Node
		node, err = getRequestBodyNode(c)
		if err == nil {
			if node.TypeSafe() != ast.V_OBJECT {
				return "", &publicVideoRequestValidationError{
					code:     "invalid_parameter",
					message:  "request body must be a JSON object",
					param:    "body",
					value:    requestJSONTypeName(node.TypeSafe()),
					expected: "JSON object",
				}
			}
			rawCapability, err = getStringFieldFromNode(
				node,
				"capability",
				"capability must be a non-empty string",
			)
		}
	}
	if err != nil {
		return "", &publicVideoRequestValidationError{
			code:     "invalid_parameter",
			message:  "capability must be a non-empty string",
			param:    "capability",
			expected: "non-empty string",
		}
	}

	rawCapability = strings.TrimSpace(rawCapability)
	if rawCapability == "" {
		return "", &publicVideoRequestValidationError{
			code:          "missing_capability",
			message:       "capability is required",
			param:         "capability",
			allowedValues: supportedVideoCapabilities,
			expected:      "non-empty string",
		}
	}

	capability := model.ModelCapability(rawCapability)
	if !capability.Valid() {
		return "", &publicVideoRequestValidationError{
			code:          "unsupported_capability",
			message:       fmt.Sprintf("unsupported video capability `%s`", rawCapability),
			param:         "capability",
			value:         rawCapability,
			allowedValues: supportedVideoCapabilities,
		}
	}

	return capability, nil
}

func setVideoCapabilityContext(
	c *gin.Context,
	publicModel string,
	routingModel string,
	capability model.ModelCapability,
) {
	c.Set(PublicRequestModel, publicModel)
	c.Set(RoutingModel, routingModel)
	c.Set(VideoCapability, string(capability))
}

func GetPublicRequestModel(c *gin.Context) string {
	if value := c.GetString(PublicRequestModel); value != "" {
		return value
	}
	return GetRequestModel(c)
}

func GetRoutingModel(c *gin.Context) string {
	if value := c.GetString(RoutingModel); value != "" {
		return value
	}
	return GetRequestModel(c)
}

func GetVideoCapability(c *gin.Context) string {
	return c.GetString(VideoCapability)
}

func publicVideoModelID(routingModel string) string {
	if publicModel, _, ok := model.ParseModelCapabilityKey(routingModel); ok {
		return publicModel
	}
	return routingModel
}
