package fal_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

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
			if _, writeErr := w.Write(
				[]byte(
					`{"images":[{"url":"https://cdn.example/image.png","content_type":"image/png"}]}`,
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
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
		case "GET /fal-ai/bytedance/seedream/v4.5/text-to-image/requests/abc":
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
	require.Len(t, calls, 3)

	for _, unsafe := range []string{"fal-ai/../evil", "fal-ai/v4..5/edit", "fal-ai/v4.5?key=secret"} {
		_, err := client.Submit(t.Context(), unsafe, []byte(`{}`))
		require.Error(t, err)
	}

	require.Len(t, calls, 3)
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
	for _, code := range []int{400, 401, 422, 429, 500, 503} {
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
			require.Equal(t, code < 500, errors.Is(err, adaptor.ErrImageSubmissionRejected))
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
