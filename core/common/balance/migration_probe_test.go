//nolint:testpackage // White-box coverage needs the package-private checkedType fixture or implementation.
package balance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func probeBillingWireFixture() (model.MigrationProbeBillingRequest, map[string]any) {
	r := model.MigrationProbeBillingRequest{
		MigrationOperationID: "op",
		FromKeyDigest:        strings.Repeat("a", 64),
		BillingOperationID:   "bill",
		TaskID:               "task",
		UpstreamTaskID:       "upstream",
		GroupID:              "group",
		Provider:             "fal",
		Endpoint:             "fal-ai/fixture",
		CredentialScope:      "credential",
		KeyDigest:            strings.Repeat("b", 64),
		QuoteHash:            strings.Repeat("c", 64),
	}
	evidence := map[string]any{
		"binding":                       r,
		"evidenceId":                    "event",
		"provenanceHash":                strings.Repeat("d", 64),
		"accountMembershipEvidenceHash": strings.Repeat("e", 64),
		"accountMembershipEvidenceKind": "fal-workspace-key-membership-v1",
		"currency":                      "USD",
		"costMicros":                    "0",
		"customerDueMicros":             "0",
		"chargedMicros":                 "0",
		"settledAtMs":                   "1770000000000",
	}

	return r, map[string]any{
		"code":    0,
		"message": "ok",
		"data": map[string]any{
			"version":  1,
			"source":   "app-migration-probe-billing-v1",
			"state":    "ready",
			"reason":   "",
			"evidence": evidence,
		},
	}
}

func TestMigrationProbeBillingHTTPStrictResponse(t *testing.T) {
	for _, mode := range []string{"ready-zero", "ready-large", "pending", "missing_provenance", "conflict", "unavailable", "wrong-binding", "amount-number", "amount-plus", "amount-leading-zero", "amount-overflow", "amount-negative", "different-charged", "missing-membership", "wrong-kind", "wrong-source", "extra-field", "ambiguous", "oversized", "trailing", "raw-error", "404", "status-evidence"} {
		t.Run(mode, func(t *testing.T) {
			req, body := probeBillingWireFixture()
			data := checkedType[map[string]any](t, body["data"])

			evidence := checkedType[map[string]any](t, data["evidence"])
			switch mode {
			case "ready-large":
				evidence["costMicros"] = "9007199254740991"
				evidence["customerDueMicros"] = "9007199254740991"
				evidence["chargedMicros"] = "9007199254740991"
			case "pending", "missing_provenance", "conflict", "unavailable":
				data["state"] = mode
				data["reason"] = "safe_reason"
				data["evidence"] = nil
			case "wrong-binding":
				bad := req
				bad.TaskID = "other"
				evidence["binding"] = bad
			case "amount-number":
				evidence["costMicros"] = 0
			case "amount-plus":
				evidence["costMicros"] = "+1"
			case "amount-leading-zero":
				evidence["costMicros"] = "01"
			case "amount-overflow":
				evidence["costMicros"] = "9007199254740992"
			case "amount-negative":
				evidence["costMicros"] = "-1"
			case "different-charged":
				evidence["chargedMicros"] = "1"
			case "missing-membership":
				delete(evidence, "accountMembershipEvidenceHash")
			case "wrong-kind":
				evidence["accountMembershipEvidenceKind"] = "permanent-account"
			case "wrong-source":
				data["source"] = "other"
			case "extra-field":
				evidence["secret"] = "not-a-real-secret"
			case "status-evidence":
				data["state"] = "pending"
			}

			var calls atomic.Int32

			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					require.Equal(t, migrationProbeBillingPath, r.URL.Path)
					require.Equal(t, "Bearer synthetic-internal", r.Header.Get("Authorization"))

					var actual model.MigrationProbeBillingRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&actual))
					require.Equal(t, req, actual)

					raw := mustJSON(t, body)
					switch mode {
					case "ambiguous":
						raw = []byte(`{"code":0,"code":0}`)
					case "oversized":
						raw = []byte(strings.Repeat(" ", migrationProbeBillingMaxBody+1))
					case "trailing":
						raw = append(raw, []byte(` {}`)...)
					case "raw-error":
						w.WriteHeader(http.StatusInternalServerError)

						raw = []byte("do not expose this internal response")
					case "404":
						w.WriteHeader(http.StatusNotFound)
					}

					_, _ = w.Write(raw)
				}),
			)
			defer server.Close()

			got, err := NewExternalHTTP(
				server.URL,
				"synthetic-internal",
			).ReadMigrationProbeBilling(t.Context(), req)
			require.EqualValues(t, 1, calls.Load())

			switch mode {
			case "ready-zero", "ready-large":
				require.NoError(t, err)
				require.Equal(t, req, got.Binding)
			case "pending":
				require.ErrorIs(t, err, model.ErrMigrationProbePending)
			case "missing_provenance":
				require.ErrorIs(t, err, model.ErrMigrationProbeMissingProvenance)
			default:
				require.Error(t, err)
				require.NotContains(t, err.Error(), "do not expose")
			}
		})
	}
}

func TestMigrationProbeBillingHTTPNoRedirectAndAbort(t *testing.T) {
	request, body := probeBillingWireFixture()

	var targetCalls atomic.Int32

	target := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)

				if err := json.NewEncoder(w).Encode(body); err != nil {
					panic(err)
				} /* A broken test response must fail visibly. */
			},
		),
	)
	defer target.Close()

	for _, status := range []int{307, 308} {
		server := httptest.NewServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) },
			),
		)
		_, e := NewExternalHTTP(
			server.URL,
			"synthetic",
		).ReadMigrationProbeBilling(t.Context(), request)
		require.Error(t, e)
		server.Close()
	}

	require.Zero(t, targetCalls.Load())

	entered := make(chan struct{})
	exited := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		_, e := NewExternalHTTP(server.URL, "synthetic").ReadMigrationProbeBilling(ctx, request)
		done <- e
	}()

	<-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("HTTP cancellation did not reach handler")
	}

	old := Default

	Default = NewMockGroupBalance()
	defer func() { Default = old }()

	_, e := ReadMigrationProbeBilling(t.Context(), request)
	require.True(t, errors.Is(e, model.ErrMigrationProbeMissingProvenance))
}
