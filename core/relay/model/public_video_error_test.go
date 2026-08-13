//nolint:testpackage
package model

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublicVideoErrorUsesSafeStableEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status  int
		typ     string
		code    string
		message string
	}{
		{http.StatusBadRequest, "invalid_request_error", "invalid_parameter", "The request contains an invalid parameter."},
		{http.StatusUnauthorized, "authentication_error", "invalid_api_key", "The API key is missing or invalid."},
		{http.StatusForbidden, "permission_error", "permission_denied", "The API key cannot access this resource."},
		{http.StatusNotFound, "invalid_request_error", "not_found", "The requested resource was not found."},
		{http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded", "Too many requests. Please retry later."},
		{http.StatusUnprocessableEntity, "invalid_request_error", "request_rejected", "The request could not be completed."},
		{http.StatusInternalServerError, "api_error", "internal_error", "The request could not be completed."},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			t.Parallel()

			err := PublicVideoError(tt.status)
			require.Equal(t, tt.status, err.StatusCode())

			var body OpenAIErrorResponse
			require.NoError(t, json.Unmarshal(mustMarshalError(t, err), &body))
			require.Equal(t, tt.typ, body.Error.Type)
			require.Equal(t, tt.code, body.Error.Code)
			require.Equal(t, tt.message, body.Error.Message)
		})
	}
}
