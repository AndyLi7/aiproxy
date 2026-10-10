// Offline rehearsal of source examples/defaults through the real request path.
// No network, provider execution, wallet, database or publication.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"os"
	"reflect"
)

func decode(raw []byte) any {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		panic(err)
	}
	return v
}
func main() {
	if len(os.Args) != 3 {
		panic("usage: native-request-census samples.json report.json")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var corpus struct {
		CorpusSHA string `json:"corpusSha256"`
		Results   []struct {
			Endpoint  string          `json:"endpoint"`
			SourceSHA string          `json:"sourceSha256"`
			Contract  json.RawMessage `json:"contract"`
			InputJSON *string         `json:"inputJSON"`
			Error     string          `json:"error"`
		} `json:"results"`
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		panic(err)
	}
	results := []map[string]any{}
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, row := range corpus.Results {
		if row.Endpoint == "" || seen[row.Endpoint] {
			panic("missing/duplicate endpoint")
		}
		seen[row.Endpoint] = true
		entry := map[string]any{"endpoint": row.Endpoint, "sourceSha256": row.SourceSHA, "executionVerified": false, "admissionVerified": false}
		status := "missing-source"
		if row.Error == "" {
			status = "missing-source-sample"
			if row.InputJSON != nil {
				task, compileErr := nativeresult.CompileTaskContract(row.Contract)
				var c nativeresult.TaskContract
				if err = json.Unmarshal(row.Contract, &c); err != nil {
					panic(err)
				}
				status = "contract-failed"
				if compileErr == nil {
					request, _ := json.Marshal(struct {
						Model string          `json:"model"`
						Input json.RawMessage `json:"input"`
					}{c.Model, json.RawMessage(*row.InputJSON)})
					input, inputErr := task.ValidateRequest(request)
					status = "source-sample-rejected"
					if inputErr == nil {
						status = "upstream-projection-rejected"
						upstream, upstreamErr := task.PrepareUpstreamInput(input)
						if upstreamErr == nil {
							expected := decode(input).(map[string]any)
							for key, value := range c.FixedParameters {
								expected[key] = decode(value)
							}
							if !reflect.DeepEqual(expected, decode(upstream)) {
								panic("payload changed for " + row.Endpoint)
							}
							// Every frozen control must remain server-owned, even when the client supplies its correct value.
							for key, value := range c.FixedParameters {
								fields := map[string]json.RawMessage{}
								if err = json.Unmarshal(input, &fields); err != nil {
									panic(err)
								}
								fields[key] = value
								badInput, _ := json.Marshal(fields)
								badRequest, _ := json.Marshal(map[string]any{"model": c.Model, "input": json.RawMessage(badInput)})
								if _, e := task.ValidateRequest(badRequest); e == nil {
									panic("client control accepted: " + row.Endpoint + " " + key)
								}
								if _, e := task.PrepareUpstreamInput(badInput); e == nil {
									panic("direct control accepted: " + row.Endpoint + " " + key)
								}
							}
							status = "source-sample-projection-verified"
							entry["fixedControlCount"] = len(c.FixedParameters)
						}
					}
				}
			}
		}
		entry["status"] = status
		counts[status]++
		results = append(results, entry)
	}
	report := map[string]any{"kind": "offline-source-sample-request-path", "corpusSha256": corpus.CorpusSHA, "samplesSha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "paidCalls": 0, "executionVerified": false, "admissionVerified": false, "counts": counts, "results": results}
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(os.Args[2], append(out, '\n'), 0600); err != nil {
		panic(err)
	}
	summary, _ := json.Marshal(counts)
	fmt.Println(string(summary))
}
