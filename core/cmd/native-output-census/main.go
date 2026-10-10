// Offline source-output replay; never downloads files or submits generation.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"os"
	"reflect"
	"strconv"
)

func decode(raw []byte) any {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		panic(err)
	}
	return value
}
func replace(root any, path []string, value string) {
	node := root
	for i, key := range path {
		last := i == len(path)-1
		switch n := node.(type) {
		case map[string]any:
			if last {
				n[key] = value
			} else {
				node = n[key]
			}
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(n) {
				panic("invalid artifact index")
			}
			if last {
				n[index] = value
			} else {
				node = n[index]
			}
		default:
			panic("invalid artifact path")
		}
	}
}
func main() {
	if len(os.Args) != 3 {
		panic("usage: native-output-census samples.json report.json")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var corpus struct {
		CorpusSHA string `json:"corpusSha256"`
		Direction string `json:"direction"`
		Results   []struct {
			Endpoint   string          `json:"endpoint"`
			SourceSHA  string          `json:"sourceSha256"`
			Contract   json.RawMessage `json:"contract"`
			OutputJSON *string         `json:"outputJSON"`
			Error      string          `json:"error"`
		} `json:"results"`
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		panic(err)
	}
	if corpus.Direction != "output" {
		panic("output samples required")
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
			if row.OutputJSON != nil {
				task, e := nativeresult.CompileTaskContract(row.Contract)
				status = "contract-failed"
				var contract nativeresult.TaskContract
				if err = json.Unmarshal(row.Contract, &contract); err != nil {
					panic(err)
				}
				if e == nil {
					original := []byte(*row.OutputJSON)
					out, e := task.ValidateOutput(original)
					status = "source-sample-rejected"
					if e == nil {
						if !bytes.Equal(out, original) {
							panic("raw output altered: " + row.Endpoint)
						}
						artifacts, e := nativeresult.PlanArtifacts(out, contract.Artifacts)
						status = "artifact-plan-rejected"
						if e == nil {
							owned := map[string]string{}
							expected := decode(out)
							for i, a := range artifacts {
								value := fmt.Sprintf("https://owned.invalid/v1/model-tasks/fixture/artifacts/%d", i)
								owned[a.Pointer] = value
								replace(expected, a.Path, value)
							}
							rewritten, e := nativeresult.RewriteArtifacts(out, contract.Artifacts, owned)
							if e != nil {
								panic(e)
							}
							if !reflect.DeepEqual(expected, decode(rewritten)) {
								panic("unrelated output changed: " + row.Endpoint)
							}
							status = "source-output-rewrite-verified"
							if _, e := task.ValidateOutput(rewritten); e != nil {
								status = "rewritten-output-schema-rejected"
							}
							if len(artifacts) > 0 {
								delete(owned, artifacts[0].Pointer)
								if _, e := nativeresult.RewriteArtifacts(out, contract.Artifacts, owned); e == nil {
									panic("partial archive accepted: " + row.Endpoint)
								}
							}
							entry["artifactCount"] = len(artifacts)
							entry["artifactBindings"] = contract.Artifacts
						}
					}
				}
			}
		}
		entry["status"] = status
		counts[status]++
		results = append(results, entry)
	}
	report := map[string]any{"kind": "offline-source-output-path", "corpusSha256": corpus.CorpusSHA, "samplesSha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "paidCalls": 0, "downloads": 0, "executionVerified": false, "admissionVerified": false, "counts": counts, "results": results}
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
