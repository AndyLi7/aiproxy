package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

// readRouteModes lists the read routes whose method is neither GET nor DELETE,
// keyed by method and the route pattern gin resolves before any handler runs.
// A rejection before the distributor (for example by TokenAuth) carries no
// relay mode; without this, it would count as a call while every relay failure
// on the same route is a read. GET and DELETE rows are reads by their endpoint
// (model.Log.IsCall). TestReadRouteModesMatchHandlers (router) keeps this in
// step with the handlers.
var readRouteModes = map[string]mode.Mode{
	"POST /v1/alpha/search":                  mode.AlphaSearch,
	"POST /v1/responses/:response_id/cancel": mode.ResponsesCancel,
}

// readPathRouteModes covers routes whose mode depends on the path: a Gemini
// POST on models/{file}:download is the GeminiFiles read.
var readPathRouteModes = map[string]func(*gin.Context) (mode.Mode, bool){
	"POST /v1/models/*model":     geminiPathReadMode,
	"POST /v1beta/models/*model": geminiPathReadMode,
}

func geminiPathReadMode(c *gin.Context) (mode.Mode, bool) {
	if GeminiPathRelayMode(c.Param("model")) == mode.GeminiFiles {
		return mode.GeminiFiles, true
	}

	return mode.Unknown, false
}

// GeminiPathRelayMode is the relay mode of a Gemini POST on models/*model:
// {file}:download reads a file, {model}:predictLongRunning starts a video job,
// anything else is a Gemini call. The relay handler and the request log both
// use it, so they cannot disagree.
func GeminiPathRelayMode(modelPath string) mode.Mode {
	modelPath = strings.TrimPrefix(modelPath, "/")

	if fileID, ok := strings.CutSuffix(modelPath, ":download"); ok && fileID != "" &&
		!strings.Contains(fileID, "/") {
		return mode.GeminiFiles
	}

	if _, action, ok := strings.Cut(modelPath, ":"); ok && action == "predictLongRunning" {
		return mode.GeminiVideo
	}

	return mode.Gemini
}

// ReadRouteMode returns the read mode of the matched route, if it is a read
// route that is neither GET nor DELETE.
func ReadRouteMode(c *gin.Context) (mode.Mode, bool) {
	key := c.Request.Method + " " + c.FullPath()

	if readMode, ok := readRouteModes[key]; ok {
		return readMode, true
	}

	if resolve, ok := readPathRouteModes[key]; ok {
		if readMode, ok := resolve(c); ok && model.IsReadMode(readMode) {
			return readMode, true
		}
	}

	return mode.Unknown, false
}
