package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
)

type toolsBindingSource struct {
	ModelID      string `json:"modelId"`
	CapabilityID string `json:"capabilityId"`
	DraftID      string `json:"draftId"`
	DraftVersion int    `json:"draftVersion"`
	ChannelID    string `json:"channelId"`
}
type toolsBindingInput struct {
	Model            string             `json:"model"`
	Capability       string             `json:"capability"`
	Enabled          bool               `json:"enabled"`
	Binding          nativeRouteBinding `json:"binding"`
	Source           toolsBindingSource `json:"source"`
	ExpectedRevision string             `json:"expectedRevision"`
}

func toolsBindingConfig(row model.ToolsModelBinding) (model.ModelConfig, bool) {
	var binding map[string]any
	if json.Unmarshal([]byte(row.BindingJSON), &binding) != nil {
		return model.ModelConfig{}, false
	}

	return model.ModelConfig{
		Model: row.Model,
		Type:  mode.NativeTasks,
		Config: map[model.ModelConfigKey]any{
			model.ModelConfigCapabilityKey: row.Capability,
			NativeResultConfigKey:          binding,
		},
	}, true
}

// Explicit disabled bindings override legacy tool access. Legacy API-backed
// tools remain compatible; the new records never enter API model caches.
func resolveToolsModel(name string) (model.ModelConfig, bool, error) {
	var row model.ToolsModelBinding

	err := model.DB.Where("model = ?", name).First(&row).Error
	if err == nil {
		if !row.Enabled {
			return model.ModelConfig{}, false, nil
		}

		mc, ok := toolsBindingConfig(row)

		return mc, ok, nil
	}

	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ModelConfig{}, false, err
	}

	caches := model.LoadModelCaches()
	if caches == nil {
		return model.ModelConfig{}, false, errors.New("model cache unavailable")
	}

	mc, ok := caches.EnabledModelConfigsMap[name]

	return mc, ok, nil
}

func GetToolsModelBindings(c *gin.Context) {
	var rows []model.ToolsModelBinding
	if err := model.DB.Order("model").Find(&rows).Error; err != nil {
		middleware.ErrorResponse(c, 503, "tool binding storage unavailable")
		return
	}

	items := []map[string]any{}
	for _, row := range rows {
		var source toolsBindingSource
		if json.Unmarshal([]byte(row.SourceJSON), &source) != nil {
			continue
		}

		mc, ok := toolsBindingConfig(row)
		if ok {
			_, _, _, ok = toolsDescriptor(mc)
		}

		reason := ""
		if !row.Enabled {
			reason = "disabled"
		} else if !ok {
			reason = "execution_configuration_unavailable"
		}

		items = append(
			items,
			map[string]any{
				"model":      row.Model,
				"capability": row.Capability,
				"enabled":    row.Enabled,
				"ready":      row.Enabled && ok,
				"reason":     reason,
				"revision":   row.Revision,
				"source":     source,
			},
		)
	}

	middleware.SuccessResponse(c, gin.H{"items": items})
}

// Management authentication is installed by the /api router. This compiles and
// freezes configuration only: no provider request, task, quote or wallet write.
func PutToolsModelBinding(c *gin.Context) {
	var in toolsBindingInput

	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, nativeresult.MaxBytes+8192))
	d.DisallowUnknownFields()

	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || !toolsText(in.Model, 256) ||
		in.Source.DraftVersion < 1 ||
		!toolsText(in.Source.ModelID, 191) ||
		!toolsText(in.Source.CapabilityID, 191) ||
		!toolsText(in.Source.DraftID, 191) ||
		!toolsText(in.Source.ChannelID, 191) {
		middleware.ErrorResponse(c, 400, "invalid tool execution binding")
		return
	}

	var contract nativeresult.TaskContract

	u, err := url.Parse(in.Binding.DeliveryBase)
	if json.Unmarshal(in.Binding.Contract, &contract) != nil || contract.Model != in.Model ||
		err != nil ||
		u.Scheme != "https" ||
		u.Host == "" ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" ||
		strings.Trim(u.Path, "/") != "" ||
		in.Binding.CredentialScope == "" ||
		in.Binding.Endpoint == "" {
		middleware.ErrorResponse(c, 400, "invalid tool contract identity or delivery origin")
		return
	}

	raw, err := json.Marshal(in.Binding)
	if err != nil {
		middleware.ErrorResponse(c, 400, "invalid tool execution binding")
		return
	}

	source, err := json.Marshal(in.Source)
	if err != nil {
		middleware.ErrorResponse(c, 400, "invalid tool binding source")
		return
	}

	row := model.ToolsModelBinding{
		Model:       in.Model,
		Capability:  in.Capability,
		Enabled:     in.Enabled,
		BindingJSON: string(raw),
		SourceJSON:  string(source),
	}
	mc, ok := toolsBindingConfig(row)

	_, _, _, valid := toolsDescriptor(mc)
	if !ok || !valid {
		middleware.ErrorResponse(c, 422, "tool contract or channel is unavailable")
		return
	}

	revision, err := json.Marshal([]any{in.Model, in.Capability, in.Enabled, in.Binding, in.Source})
	if err != nil {
		middleware.ErrorResponse(c, 400, "invalid tool execution binding")
		return
	}

	row.Revision = toolsDigest(revision)
	// A stale operator cannot overwrite a newer execution configuration.
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		if in.ExpectedRevision == "" {
			return tx.Create(&row).Error
		}

		result := tx.Model(&model.ToolsModelBinding{}).
			Where("model = ? AND revision = ?", in.Model, in.ExpectedRevision).
			Updates(map[string]any{"enabled": row.Enabled, "capability": row.Capability, "binding_json": row.BindingJSON, "source_json": row.SourceJSON, "revision": row.Revision})
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return errors.New("binding changed")
		}

		return nil
	})
	if err != nil {
		middleware.ErrorResponse(c, 409, "tool binding changed; refresh and retry")
		return
	}

	middleware.SuccessResponse(c, gin.H{"model": row.Model, "revision": row.Revision})
}
