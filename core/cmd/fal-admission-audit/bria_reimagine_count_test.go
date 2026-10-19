package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBriaReimagineActualSourceBatchAndGuidance(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/bria-reimagine-count.json")
	require.NoError(t, err)

	var rows []sample
	require.NoError(t, json.Unmarshal(data, &rows))
	require.Len(t, rows, 1)

	for _, count := range []int{1, 2, 4} {
		for _, guided := range []bool{false, true} {
			row := rows[0]

			var request, response map[string]any
			require.NoError(t, json.Unmarshal(row.Request, &request))
			require.NoError(t, json.Unmarshal(row.Response, &response))

			request["num_results"] = count

			actual := count
			if guided {
				request["structure_image_url"] = "https://example.com/structure.png"
				actual = 1
			}

			images := make([]any, actual)
			for i := range images {
				images[i] = map[string]any{"url": "https://example.com/result.png"}
			}

			response["images"] = images
			row.Request = marshalFixture(t, request)
			row.Response = marshalFixture(t, response)
			result := run(row)
			require.True(t, result.Passed, result.Error)
			require.True(t, result.OutputPassed, result.OutputError)
			// Even valid image objects must not bypass the request's declared batch ceiling.
			response["images"] = append(images, images...)
			if guided {
				response["images"] = make([]any, count+1)
				for i := range testValue[[]any](t, response["images"]) {
					testValue[[]any](t, response["images"])[i] = images[0]
				}
			}

			row.Response = marshalFixture(t, response)
			result = run(row)
			require.False(t, result.OutputPassed)
		}
	}

	for _, bad := range []any{0, 5, 1.5, "4"} {
		row := rows[0]

		var request map[string]any
		require.NoError(t, json.Unmarshal(row.Request, &request))
		request["num_results"] = bad
		row.Request = marshalFixture(t, request)
		require.False(t, run(row).Passed)
	}
}

func TestBriaPrimaryCountWithAllGuidanceMethods(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/bria-guidance-count.json")
	require.NoError(t, err)

	var rows []sample
	require.NoError(t, json.Unmarshal(data, &rows))
	require.Len(t, rows, 3)

	methods := []string{
		"controlnet_canny",
		"controlnet_depth",
		"controlnet_recoloring",
		"controlnet_color_grid",
	}
	for _, source := range rows {
		for _, count := range []int{1, 2, 4} {
			for size := range 5 {
				row := source

				var request, response map[string]any
				require.NoError(t, json.Unmarshal(row.Request, &request))
				require.NoError(t, json.Unmarshal(row.Response, &response))

				image := testValue[map[string]any](t, testValue[[]any](t, request["guidance"])[0])["image_url"]

				guidance := make([]any, size)
				for i := range guidance {
					guidance[i] = map[string]any{
						"image_url": image,
						"method":    methods[i],
						"scale":     1,
					}
				}

				request["guidance"] = guidance
				request["n"] = count

				outputs := count
				if size > 0 {
					outputs = 1
				}

				images := make([]any, outputs)
				for i := range images {
					images[i] = map[string]any{"url": "https://example.com/result.png"}
				}

				response["images"] = images
				row.Request = marshalFixture(t, request)
				row.Response = marshalFixture(t, response)
				result := run(row)
				require.True(t, result.Passed, source.Endpoint, result.Error)
				require.True(t, result.OutputPassed, source.Endpoint, result.OutputError)
			}
		}
	}
}
