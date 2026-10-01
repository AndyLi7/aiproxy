// Offline schema census. Does not submit, migrate, route, publish or charge.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"os"
)

type row struct {
	Endpoint       string          `json:"endpoint"`
	SourceSHA256   string          `json:"sourceSha256,omitempty"`
	RequestSchema  json.RawMessage `json:"requestSchema,omitempty"`
	ResponseSchema json.RawMessage `json:"responseSchema,omitempty"`
	Contract       json.RawMessage `json:"contract,omitempty"`
	Error          string          `json:"error,omitempty"`
}

func main() {
	if len(os.Args) != 3 {
		panic("usage: native-result-census input.json output.json")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var rows []row
	if err = json.Unmarshal(raw, &rows); err != nil {
		panic(err)
	}
	results := []map[string]any{}
	passed := 0
	sourceErrors := 0
	for _, r := range rows {
		entry := map[string]any{"endpoint": r.Endpoint, "sourceSha256": r.SourceSHA256, "schemaCompiled": false, "publicReady": false}
		if r.Error != "" {
			entry["error"] = r.Error
			sourceErrors++
		} else if _, err = nativeresult.CompileTaskContract(r.Contract); err != nil {
			entry["error"] = err.Error()
		} else if _, err = nativeresult.Compile(r.RequestSchema); err != nil {
			entry["error"] = "documentation schema: " + err.Error()
		} else if len(r.ResponseSchema) > 0 {
			if _, err = nativeresult.Compile(r.ResponseSchema); err != nil {
				entry["error"] = "response documentation schema: " + err.Error()
			} else {
				entry["schemaCompiled"] = true
				entry["responseSchemaCompiled"] = true
				passed++
			}
		} else {
			entry["schemaCompiled"] = true
			passed++
		}
		results = append(results, entry)
	}
	report := map[string]any{"summary": map[string]any{"total": len(rows), "schemaCompiled": passed, "sourceFailures": sourceErrors, "schemaFailures": len(rows) - passed - sourceErrors, "publicReady": 0}, "results": results}
	output, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(os.Args[2], output, 0600); err != nil {
		panic(err)
	}
	summary, _ := json.Marshal(report["summary"])
	fmt.Println(string(summary))
}
