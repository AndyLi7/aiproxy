package fal_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
)

func TestQueueLifecycle(t *testing.T) {
	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Key secret", r.Header.Get("Authorization"))

		switch r.URL.Path {
		case "/fal-ai/minimax/image-01":
			calls++

			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, float64(1), body["num_images"])
			require.NotContains(t, body, "n")
			require.NotContains(t, body, "model")

			if _, writeErr := w.Write([]byte(`{"request_id":"abc"}`)); writeErr != nil {
				t.Errorf("write mock response: %v", writeErr)
			}
		case "/fal-ai/minimax/requests/abc/status":
			if _, writeErr := w.Write([]byte(`{"status":"COMPLETED"}`)); writeErr != nil {
				t.Errorf("write mock response: %v", writeErr)
			}
		case "/fal-ai/minimax/requests/abc":
			w.Header().Set("x-fal-billable-units", "1.5")
			if _, writeErr := w.Write(
				[]byte(
					`{"num_images":1,"description":"generated caption","seed":42,"prompt":"revised description","images":[{"url":"https://cdn.example/image.png","content_type":"image/png"}]}`,
				),
			); writeErr != nil {
				t.Errorf("write mock response: %v", writeErr)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "secret"}
	id, err := client.Submit(
		t.Context(),
		"fal-ai/minimax/image-01",
		[]byte(`{"prompt":"test","num_images":1}`),
	)
	require.NoError(t, err)
	require.Equal(t, "abc", id)
	result, err := client.Poll(t.Context(), "fal-ai/minimax/image-01", id)
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Len(t, result.Data, 1)
	require.Equal(t, int64(42), *result.Metadata.Seed)
	require.Equal(t, "revised description", result.Metadata.Prompt)
	require.Equal(t, "1.5", result.Metadata.BillableUnits)
	require.Equal(t, "generated caption", result.Metadata.Description)
	require.Equal(t, int64(1), *result.Metadata.NumImages)
	require.Equal(t, "revised description", result.Data[0].RevisedPrompt)
	require.Equal(t, 1, calls)
}

func TestDottedSeedreamFullQueuePath(t *testing.T) {
	raw, err := os.ReadFile(
		"../../../common/registryvalidation/testdata/seedream-4.5-v2-text-to-image.json",
	)
	require.NoError(t, err)

	var contract struct {
		Providers map[string]struct {
			Upstream registryvalidation.ProviderSpec `json:"upstream"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &contract))
	spec := contract.Providers["fal"].Upstream
	binding := registryvalidation.ProviderBinding{
		Provider:     "fal",
		ID:           spec.ID,
		Revision:     spec.Revision,
		ContractHash: spec.ContractHash,
	}
	frozen, err := registryvalidation.FreezeProviderBinding(raw, binding)
	require.NoError(t, err)

	calls := []string{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "POST /fal-ai/bytedance/seedream/v4.5/text-to-image":
			_, _ = w.Write([]byte(`{"request_id":"abc"}`))
		case "GET /fal-ai/bytedance/seedream/v4.5/text-to-image/requests/abc/status":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "GET /fal-ai/bytedance/requests/abc/status":
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
		case "GET /fal-ai/bytedance/requests/abc":
			_, _ = w.Write(
				[]byte(
					`{"images":[{"url":"https://fal.media/out.png","width":null,"height":null}],"seed":42}`,
				),
			)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "secret"}
	id, err := client.Submit(
		t.Context(),
		spec.Endpoint,
		[]byte(`{"prompt":"test","num_images":1,"max_images":1}`),
	)
	require.NoError(t, err)
	result, err := client.Poll(t.Context(), spec.Endpoint, id, frozen)
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Len(t, result.Data, 1)
	require.Len(t, calls, 4)

	for _, unsafe := range []string{"fal-ai/../evil", "fal-ai/v4..5/edit", "fal-ai/v4.5?key=secret"} {
		_, err := client.Submit(t.Context(), unsafe, []byte(`{}`))
		require.Error(t, err)
	}

	require.Len(t, calls, 4)
}

func TestWanFullQueuePathFallsBackAfterMethodRejection(t *testing.T) {
	raw, err := os.ReadFile(
		"../../../common/registryvalidation/testdata/seedream-4.5-v2-text-to-image.json",
	)
	require.NoError(t, err)
	raw = []byte(strings.ReplaceAll(
		string(raw),
		"fal-ai/bytedance/seedream/v4.5/text-to-image",
		"wan/v2.6/text-to-image",
	))

	var contract struct {
		Providers map[string]struct {
			Upstream registryvalidation.ProviderSpec `json:"upstream"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &contract))
	spec := contract.Providers["fal"].Upstream
	frozen, err := registryvalidation.FreezeProviderBinding(raw, registryvalidation.ProviderBinding{
		Provider:     "fal",
		ID:           spec.ID,
		Revision:     spec.Revision,
		ContractHash: spec.ContractHash,
	})
	require.NoError(t, err)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/wan/v2.6/text-to-image/requests/abc/status":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "/wan/v2.6/requests/abc/status":
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
		case "/wan/v2.6/requests/abc":
			_, _ = w.Write([]byte(`{"images":[{"url":"https://fal.media/out.png","width":null,"height":null}],"seed":42}`))
		default:
			t.Errorf("unexpected queue path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "secret"}
	result, err := client.Poll(t.Context(), spec.Endpoint, "abc", frozen)
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Len(t, result.Data, 1)
	require.Equal(t, []string{
		"GET /wan/v2.6/text-to-image/requests/abc/status",
		"GET /wan/v2.6/requests/abc/status",
		"GET /wan/v2.6/requests/abc",
	}, calls)
}

func TestRejectInvalidSuccess(t *testing.T) {
	for _, body := range []string{`{"images":[]}`, `{"images":[{"url":"http://cdn.example/a"}]}`, `{"images":[{"url":"https://user:pass@cdn.example/a"}]}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path[len(r.URL.Path)-6:] == "status" {
					if _, writeErr := w.Write([]byte(`{"status":"COMPLETED"}`)); writeErr != nil {
						t.Errorf("write mock response: %v", writeErr)
					}
				} else {
					if _, writeErr := w.Write([]byte(body)); writeErr != nil {
						t.Errorf("write mock response: %v", writeErr)
					}
				}
			}))
			defer s.Close()

			c := fal.Client{HTTP: s.Client(), BaseURL: s.URL}
			result, err := c.Poll(t.Context(), "fal-ai/minimax/image-01", "abc")
			require.NoError(t, err)
			require.Equal(t, "failed", result.Status)
			require.Empty(t, result.Data)
		})
	}
}

func TestFalRejectsSynchronousRelay(t *testing.T) {
	a := &fal.Adaptor{}
	_, err := a.GetRequestURL(nil, nil, nil)
	require.Error(t, err)
}

func TestFalPollingErrorsAndNoCredentialRedirect(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		want       string
	}{
		{"queued", `{"status":"IN_QUEUE"}`, 200, "queued"},
		{"running", `{"status":"IN_PROGRESS"}`, 200, "in_progress"},
		{"terminal", `{"status":"COMPLETED","error":"private upstream details"}`, 200, "failed"},
		{"transient", `{}`, 503, ""},
		{"redirect", `{}`, 307, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0

			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++

				w.Header().Set("Location", "http://invalid.example/leak")
				w.WriteHeader(tc.code)

				if _, writeErr := w.Write([]byte(tc.body)); writeErr != nil {
					t.Errorf("write mock response: %v", writeErr)
				}
			}))
			defer s.Close()

			c := fal.Client{HTTP: s.Client(), BaseURL: s.URL, Key: "secret"}

			result, err := c.Poll(t.Context(), "fal-ai/minimax/image-01", "abc")
			if tc.want == "" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, result.Status)
			}

			require.Equal(t, 1, calls)
		})
	}
}

func TestSubmissionRejectionIsDistinctFromUnknownAcceptance(t *testing.T) {
	for _, code := range []int{302, 400, 401, 402, 403, 404, 408, 413, 422, 429, 451, 500, 502, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			s := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) },
				),
			)
			defer s.Close()

			c := fal.Client{HTTP: s.Client(), BaseURL: s.URL}
			_, err := c.Submit(
				t.Context(),
				"fal-ai/minimax/image-01",
				[]byte(`{"prompt":"test","n":1}`),
			)
			require.Error(t, err)
			// Owner rule 2026-10-08: input rejections and provider-unavailable
			// statuses (credential, payment or exhausted balance, unknown
			// endpoint, throttling) prove fal created no request. Server errors,
			// timeouts and redirects do not establish whether it accepted work.
			rejected := code == 400 || code == 413 || code == 422 || code == 451
			unavailable := code == 401 || code == 402 || code == 403 || code == 404 || code == 429
			require.Equal(t, rejected || unavailable, errors.Is(err, adaptor.ErrImageSubmissionRejected))
			require.Equal(t, unavailable, adaptor.ProviderUnavailable(err))
			failure := failover.FromError(err)
			status, _ := adaptor.SubmissionEvidence(err)
			require.Equal(t, code, status)
			switch {
			case rejected:
				require.Equal(t, failover.NotAccepted, failure.Acceptance)
				require.Equal(t, failover.InvalidRequest, failure.Class)
			case unavailable:
				require.Equal(t, failover.NotAccepted, failure.Acceptance)
				require.Equal(t, failover.Transient, failure.Class)
				require.Equal(t, "upstream_unavailable", adaptor.NotAcceptedTaskError(err).Code)
			default:
				require.Equal(t, failover.Unknown, failure.Acceptance)
			}
		})
	}
}

// The 2026-10-08 incident: fal answered an exhausted account with 403. The
// task is not accepted, and operators get the status and fal's own reason,
// never the API key or the customer's prompt.
func TestSubmitCarriesSanitizedProviderEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, want, absent string
		code                     int
	}{
		{"exhausted balance", `{"detail":"User is locked. Reason: Exhausted balance. Top up your balance at fal.ai/dashboard/billing."}`, "User is locked. Reason: Exhausted balance.", "", 403},
		{"echoed credential", `{"detail":"invalid key fal-key-0123456789"}`, "invalid key [redacted]", "fal-key-0123456789", 401},
		{"input rejection", `{"detail":[{"type":"string_too_long","loc":["body","prompt"],"input":"secret customer prompt"}]}`, "string_too_long@body.prompt", "secret customer prompt", 422},
		{"server error", `<html>bad gateway</html>`, "<html>bad gateway</html>", "", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c := fal.Client{HTTP: s.Client(), BaseURL: s.URL, Key: "fal-key-0123456789"}
			_, err := c.SubmitNative(t.Context(), "fal-ai/qwen-image", []byte(`{"prompt":"secret customer prompt"}`))
			status, reason := adaptor.SubmissionEvidence(err)
			require.Equal(t, tc.code, status)
			require.Contains(t, reason, tc.want)
			require.NotContains(t, reason, "secret customer prompt")
			if tc.absent != "" {
				require.NotContains(t, reason, tc.absent)
			}
		})
	}
}

func TestSubjectReferenceMapsAndSubmitsURLOrData(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/fal-minimax-subject-reference-contract.json")
	require.NoError(t, err)

	var wrapper struct {
		Contract json.RawMessage `json:"contract"`
	}
	require.NoError(t, json.Unmarshal(raw, &wrapper))

	var contract any
	require.NoError(t, json.Unmarshal(raw, &contract))

	config := map[model.ModelConfigKey]any{"x_token_platform_capability_contract": contract}
	for _, reference := range []string{"https://cdn.example/subject.png", "data:image/png;base64,aGVsbG8="} {
		t.Run(reference, func(t *testing.T) {
			input, err := json.Marshal(
				map[string]any{
					"model":     "minimax/image-01/subject-reference",
					"prompt":    "portrait",
					"n":         9,
					"image_url": reference,
				},
			)
			require.NoError(t, err)

			normalized, validationErr := registryvalidation.ValidateImage(
				wrapper.Contract,
				"minimax/image-01/subject-reference",
				input,
			)
			require.Nil(t, validationErr)

			mapped, err := adaptor.MapImageProviderInput(config, "fal-image", normalized)
			require.NoError(t, err)

			calls := 0

			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++

					require.Equal(t, "/fal-ai/minimax/image-01/subject-reference", r.URL.Path)
					require.Equal(t, http.MethodPost, r.Method)

					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, reference, body["image_url"])
					require.Equal(t, float64(9), body["num_images"])
					require.NotContains(t, body, "n")
					require.NotContains(t, body, "model")

					if _, err := w.Write([]byte(`{"request_id":"subject"}`)); err != nil {
						t.Errorf("write response: %v", err)
					}
				}),
			)
			defer server.Close()

			client := fal.Client{HTTP: server.Client(), BaseURL: server.URL}
			id, err := client.Submit(
				t.Context(),
				"fal-ai/minimax/image-01/subject-reference",
				mapped,
			)
			require.NoError(t, err)
			require.Equal(t, "subject", id)
			require.Equal(t, 1, calls)
		})
	}
}

func TestPollFrozenOutputContract(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatal("poll must never submit")
		}

		if r.URL.Path == "/fal-ai/test/requests/abc/status" {
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
			return
		}

		_, _ = w.Write([]byte(`{"images":[{"url":"https://example.com/image.png"}]}`))
	}))
	defer s.Close()

	contract := []byte(
		`{"provider_contract_version":1,"selected_provider_binding":{"provider":"p","id":"p","revision":"1","contractHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"providers":{"p":{"adapter":"fal-image","upstream":{"id":"p","revision":"1","contractHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","endpoint":"fal-ai/test","execution":{"mode":"async","statusEndpoint":"fal-ai/test/requests/{request_id}/status","resultEndpoint":"fal-ai/test/requests/{request_id}","supportsCancellation":false},"outputJsonSchema":{"type":"object","required":["usage"]}}}}}`,
	)
	c := fal.Client{BaseURL: s.URL}
	result, err := c.Poll(t.Context(), "fal-ai/test", "abc", contract)
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, "invalid_result", result.Error.Code)
}

func TestFrozenFullQueuePaths(t *testing.T) {
	raw, err := os.ReadFile("../../../common/registryvalidation/testdata/provider.json")
	require.NoError(t, err)

	var c map[string]any
	require.NoError(t, json.Unmarshal(raw, &c))
	providers, ok := c["providers"].(map[string]any)
	require.True(t, ok)
	small, ok := providers["small"].(map[string]any)
	require.True(t, ok)
	s, ok := small["upstream"].(map[string]any)
	require.True(t, ok)

	s["endpoint"] = "bytedance/seedream/v5/pro/edit"
	e, ok := s["execution"].(map[string]any)
	require.True(t, ok)

	e["statusEndpoint"] = "bytedance/seedream/v5/pro/edit/requests/{request_id}/status"
	e["resultEndpoint"] = "bytedance/seedream/v5/pro/edit/requests/{request_id}"
	e["supportsCancellation"] = true
	raw, err = json.Marshal(c)
	require.NoError(t, err)
	frozen, err := registryvalidation.FreezeProviderBinding(
		raw,
		registryvalidation.ProviderBinding{
			Provider:     "small",
			ID:           "small",
			Revision:     "1",
			ContractHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	)
	require.NoError(t, err)

	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		require.Equal(t, http.MethodGet, r.Method)

		switch r.URL.Path {
		case "/bytedance/seedream/v5/pro/edit/requests/abc/status":
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
		case "/bytedance/seedream/v5/pro/edit/requests/abc":
			_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn.example/a.png"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := fal.Client{HTTP: server.Client(), BaseURL: server.URL}
	result, err := client.Poll(t.Context(), "bytedance/seedream/v5/pro/edit", "abc", frozen)
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Equal(t, 2, calls)
}

func TestFalNormalizesSingleImageWithoutWeakeningValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, status string }{
		{"single", `{"image":{"url":"https://cdn.example/a.png","content_type":"image/png"}}`, "completed"},
		{"provider cannot set delivery state", `{"image":{"url":"https://cdn.example/a.png","stored":true,"url_expires_at":"2030-01-01T00:00:00Z","auxiliary_images":{"mask_image":{"url":"https://private.test/secret","stored":true}}}}`, "completed"},
		{"empty", `{"image":null}`, "failed"},
		{"unsafe", `{"image":{"url":"http://cdn.example/a.png"}}`, "failed"},
		{"ambiguous", `{"image":{"url":"https://cdn.example/a.png"},"images":[{"url":"https://cdn.example/b.png"}]}`, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/status") {
					_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
				} else {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer s.Close()
			c := fal.Client{HTTP: s.Client(), BaseURL: s.URL}
			result, err := c.Poll(t.Context(), "fal-ai/test", "abc")
			require.NoError(t, err)
			require.Equal(t, tc.status, result.Status)
			if tc.status == "completed" {
				require.Len(t, result.Data, 1)
				require.Equal(t, "https://cdn.example/a.png", result.Data[0].URL)
				require.False(t, result.Data[0].Stored)
				require.Nil(t, result.Data[0].URLExpiresAt)
				require.Nil(t, result.Data[0].AuxiliaryImages)
			} else {
				require.Empty(t, result.Data)
			}
		})
	}
}

func TestSingleImageHonorsFrozenNativeOutput(t *testing.T) {
	for _, required := range []string{"image", "images"} {
		t.Run(required, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/status") {
					_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
					return
				}
				_, _ = w.Write([]byte(`{"image":{"url":"https://cdn.example/a.png"}}`))
			}))
			defer s.Close()
			contract := []byte(`{"provider_contract_version":1,"selected_provider_binding":{"provider":"p","id":"p","revision":"1","contractHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"providers":{"p":{"adapter":"fal-image","upstream":{"id":"p","revision":"1","contractHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","endpoint":"fal-ai/test","execution":{"mode":"async","statusEndpoint":"fal-ai/test/requests/{request_id}/status","resultEndpoint":"fal-ai/test/requests/{request_id}","supportsCancellation":false},"outputJsonSchema":{"type":"object","required":["` + required + `"],"properties":{"image":{"type":"object","required":["url"],"properties":{"url":{"type":"string"}}}}}}}}}`)
			require.True(t, json.Valid(contract), string(contract))
			c := fal.Client{HTTP: s.Client(), BaseURL: s.URL}
			result, err := c.Poll(t.Context(), "fal-ai/test", "abc", contract)
			require.NoError(t, err)
			if required == "image" {
				require.Equal(t, "completed", result.Status)
				require.Len(t, result.Data, 1)
			} else {
				require.Equal(t, "failed", result.Status)
				require.Empty(t, result.Data)
			}
		})
	}
}

func TestCompletedQueueDistinguishesRunnerFailureFromGatewayTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		terminal   bool
	}{
		{"runner failure", `{"detail":[{"type":"downstream_service_unavailable","msg":"Downstream service unavailable"}]}`, true},
		{"gateway timeout", `<html>Gateway timeout</html>`, false},
		{"unknown error", `{"detail":[{"type":"unknown"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, http.MethodGet, r.Method)
				if strings.HasSuffix(r.URL.Path, "/status") {
					_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
					return
				}
				w.WriteHeader(http.StatusGatewayTimeout)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
			result, err := client.Poll(t.Context(), "wan/v2.6/text-to-image", "test-id")
			if tc.terminal {
				require.NoError(t, err)
				require.Equal(t, "failed", result.Status)
				require.Equal(t, "upstream_failed", result.Error.Code)
			} else {
				require.Error(t, err)
				require.Empty(t, result.Status)
			}
			require.Equal(t, 2, calls)
		})
	}
}

func TestNativeOutputSeedsAndActualPrompt(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status string
	}{
		{`{"seeds":[0,42],"actual_prompt":"expanded","images":[{"url":"https://cdn.example/a.png"},{"url":"https://cdn.example/b.png"}]}`, "completed"},
		{`{"seeds":[42],"images":[{"url":"https://cdn.example/a.png"},{"url":"https://cdn.example/b.png"}]}`, "failed"},
		{`{"seeds":[],"images":[{"url":"https://cdn.example/a.png"}]}`, "failed"},
		{`{"actual_prompt":"new","images":[{"url":"https://cdn.example/a.png","revised_prompt":"different"}]}`, "failed"},
		{`{"seeds":[2],"images":[{"url":"https://cdn.example/a.png","seed":1}]}`, "failed"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/status") {
				_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
				return
			}
			_, _ = w.Write([]byte(tc.body))
		}))
		client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
		result, err := client.Poll(t.Context(), "fal-ai/wan-25-preview/text-to-image", "abc")
		server.Close()
		require.NoError(t, err)
		require.Equal(t, tc.status, result.Status)
		if tc.status == "completed" {
			require.Equal(t, json.Number("0"), *result.Data[0].Seed)
			require.Equal(t, json.Number("42"), *result.Data[1].Seed)
			require.Equal(t, "expanded", result.Metadata.Prompt)
			require.Equal(t, "expanded", result.Data[0].RevisedPrompt)
		}
	}
}

func TestOptionalOutputImageCount(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		count       *int64
	}{
		{name: "absent"}, {name: "null", field: `"num_images":null,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/status") {
					_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
					return
				}
				_, _ = w.Write([]byte(`{` + tc.field + `"images":[{"url":"https://cdn.example/image.png"}]}`))
			}))
			defer server.Close()
			client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
			result, err := client.Poll(t.Context(), "fal-ai/test/image", "abc")
			require.NoError(t, err)
			require.Equal(t, "completed", result.Status)
			require.Nil(t, result.Metadata.NumImages)
		})
	}
}

func TestNullableActualPromptProjection(t *testing.T) {
	for _, tc := range []struct{ name, field, want string }{
		{"absent", "", ""},
		{"null", `,"actual_prompt":null`, ""},
		{"empty", `,"actual_prompt":""`, ""},
		{"text", `,"actual_prompt":"expanded prompt"`, "expanded prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/status") {
					_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
					return
				}
				_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn.example/a.png"}]` + tc.field + `}`))
			}))
			defer server.Close()
			client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
			result, err := client.Poll(t.Context(), "fal-ai/test", "test-id")
			require.NoError(t, err)
			require.Equal(t, "completed", result.Status)
			require.Len(t, result.Data, 1)
			require.Equal(t, tc.want, result.Data[0].RevisedPrompt)
		})
	}
}

func TestRootRevisedPromptProjection(t *testing.T) {
	for _, tc := range []struct{ field, status, want string }{
		{`,"revised_prompt":"expanded"`, "completed", "expanded"},
		{`,"revised_prompt":null`, "completed", ""},
		{`,"revised_prompt":""`, "completed", ""},
		{`,"revised_prompt":"","prompt":"original"`, "completed", ""},
		{`,"revised_prompt":"expanded","actual_prompt":"expanded"`, "completed", "expanded"},
		{`,"revised_prompt":"expanded","actual_prompt":"different"`, "failed", ""},
		{`,"revised_prompt":"","actual_prompt":"different"`, "failed", ""},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/status") {
				_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
				return
			}
			_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn.example/a.png"},{"url":"https://cdn.example/b.png"}]` + tc.field + `}`))
		}))
		client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
		result, err := client.Poll(t.Context(), "xai/grok-imagine-image", "id")
		server.Close()
		require.NoError(t, err)
		require.Equal(t, tc.status, result.Status)
		if result.Status == "completed" {
			require.Len(t, result.Data, 2)
			for _, img := range result.Data {
				require.Equal(t, tc.want, img.RevisedPrompt)
			}
		}
	}
}

func TestUsedSeedAliasPreservesExactNumbers(t *testing.T) {
	for _, tc := range []struct{ fields, status, want string }{
		{`,"used_seed":0`, "completed", "0"},
		{`,"used_seed":-1`, "completed", "-1"},
		{`,"used_seed":18446744073709551615`, "completed", "18446744073709551615"},
		{`,"used_seed":42,"seed":42`, "completed", "42"},
		{`,"used_seed":42,"seed":43`, "failed", ""},
		{`,"used_seed":1.5`, "failed", ""},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/status") {
				_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
				return
			}
			_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn.example/a.png"}]` + tc.fields + `}`))
		}))
		client := fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
		result, err := client.Poll(t.Context(), "fal-ai/finegrain-eraser", "id")
		server.Close()
		require.NoError(t, err)
		require.Equal(t, tc.status, result.Status)
		if result.Status == "completed" {
			if result.Metadata.Seed != nil {
				value, _ := json.Marshal(*result.Metadata.Seed)
				require.Equal(t, tc.want, string(value))
			} else {
				require.Equal(t, tc.want, result.Metadata.SeedExact)
			}
		}
	}
}
