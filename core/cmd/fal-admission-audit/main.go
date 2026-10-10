// Offline replay of compiled app contracts. Never calls a provider or database.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"io"
	"net/http"
	"os"
	"strings"
)

type sample struct {
	Endpoint          string          `json:"endpoint"`
	Scenario          string          `json:"scenario"`
	Contract          json.RawMessage `json:"contract"`
	Request           json.RawMessage `json:"request"`
	Response          json.RawMessage `json:"response"`
	OutputSampleError string          `json:"outputSampleError"`
	SampleError       string          `json:"sampleError"`
}
type result struct {
	Endpoint     string `json:"endpoint"`
	Scenario     string `json:"scenario"`
	Stage        string `json:"stage"`
	Passed       bool   `json:"passed"`
	OutputStage  string `json:"outputStage"`
	OutputPassed bool   `json:"outputPassed"`
	OutputError  string `json:"outputError,omitempty"`
	Error        any    `json:"error,omitempty"`
}

func run(s sample) result {
	r := result{Endpoint: s.Endpoint, Scenario: s.Scenario, Stage: "sample_generation"}
	if s.SampleError != "" {
		r.Error = s.SampleError
		return r
	}
	var c struct {
		ID        string `json:"entry_id"`
		Providers map[string]struct {
			Upstream rv.ProviderSpec `json:"upstream"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(s.Contract, &c); err != nil {
		r.Error = err.Error()
		return r
	}
	r.Stage = "public_validation"
	normalized, ve := rv.ValidateImage(s.Contract, c.ID, s.Request)
	if ve != nil {
		r.Error = ve.PublicError()
		return r
	}
	p := c.Providers["fal"].Upstream
	b := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
	r.Stage = "provider_mapping"
	mapped, err := rv.MapBoundProviderInput(s.Contract, b, "fal-image", p.Endpoint, "async", normalized)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	var body map[string]any
	json.Unmarshal(normalized, &body)
	n := 1
	if v, ok := body["n"].(float64); ok {
		n = int(v)
	}
	r.Stage = "metering"
	evidence, err := rv.ResolveImageMetering(s.Contract, b, mapped, n, 1024, true)
	if err != nil {
		var ve *rv.ValidationError
		if errors.As(err, &ve) {
			r.Error = ve.PublicError()
		} else {
			r.Error = err.Error()
		}
		return r
	}
	r.Stage = "gateway_contract_passed"
	r.Passed = true
	r.OutputStage = "output_sample_generation"
	if s.OutputSampleError != "" || len(s.Response) == 0 {
		r.OutputError = s.OutputSampleError
		return r
	}
	frozen, err := rv.FreezeProviderBinding(s.Contract, b)
	if err != nil {
		r.OutputError = err.Error()
		return r
	}
	r.OutputStage = "output_sample_validation"
	if err = rv.ValidateFrozenProviderOutput(frozen, s.Response); err != nil {
		r.OutputError = err.Error()
		return r
	}
	r.OutputStage = "output_replay"
	if err = replayOutput(frozen, p.Endpoint, s.Response, true, evidence.MaximumOutputs); err != nil {
		r.OutputError = err.Error()
		return r
	}
	if err = replayOutput(frozen, p.Endpoint, []byte(`{}`), false); err != nil {
		r.OutputError = err.Error()
		return r
	}
	r.OutputStage = "output_replay_passed"
	r.OutputPassed = true
	return r
}
func main() {
	if len(os.Args) != 3 {
		panic("usage: fal-admission-audit cases.json results.json")
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var samples []sample
	if err = json.Unmarshal(data, &samples); err != nil {
		panic(err)
	}
	if len(samples) == 0 {
		panic("empty audit input")
	}
	rows := make([]result, 0, len(samples))
	stages := map[string]int{}
	outputStages := map[string]int{}
	for _, s := range samples {
		r := run(s)
		rows = append(rows, r)
		stages[r.Stage]++
		outputStages[r.OutputStage]++
	}
	out, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(os.Args[2], out, 0600); err != nil {
		panic(err)
	}
	summary, _ := json.Marshal(map[string]any{"input": stages, "output": outputStages})
	fmt.Println(string(summary))
	if stages["gateway_contract_passed"] != len(samples) || outputStages["output_replay_passed"] != len(samples) {
		os.Exit(1)
	}
}

// This transport has no network fallback; every response is an in-memory fixture.
type fixtureTransport struct{ body []byte }

func (t fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.URL.Host != "offline.invalid" {
		return nil, errors.New("offline replay forbids network")
	}
	body := t.body
	if strings.HasSuffix(req.URL.Path, "/status") {
		body = []byte(`{"status":"COMPLETED"}`)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}
func replayOutput(frozen []byte, endpoint string, body []byte, wantSuccess bool, maximum ...int) error {
	client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: body}}}
	result, err := client.Poll(context.Background(), endpoint, "audit", frozen)
	if wantSuccess {
		if err != nil {
			return err
		}
		if len(maximum) > 0 && len(result.Data) > maximum[0] {
			return errors.New("output count exceeds request bound")
		}
		for _, output := range result.Data {
			if !model.ValidAuxiliaryImages(output) {
				return errors.New("parsed result cannot pass archive/delivery asset validation")
			}
		}
		if result.Metadata.NumImages != nil && *result.Metadata.NumImages != int64(len(result.Data)) {
			return errors.New("declared output count differs from delivered images")
		}
		if result.Status != "completed" || len(result.Data) == 0 {
			return errors.New("valid output did not produce completed images")
		}
	}
	if !wantSuccess && err == nil && result.Status != "failed" {
		return errors.New("empty output was not rejected")
	}
	return nil
}
