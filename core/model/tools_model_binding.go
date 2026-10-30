package model

// ToolsModelBinding is an internal execution snapshot, not a sellable API
// ModelConfig. Apply the additive tools-model-bindings-v1 migration explicitly.
type ToolsModelBinding struct {
	Model       string `gorm:"primaryKey;size:256" json:"model"`
	Enabled     bool   `json:"enabled"`
	Capability  string `gorm:"size:80" json:"capability"`
	BindingJSON string `gorm:"type:text;not null" json:"-"`
	SourceJSON  string `gorm:"type:text;not null" json:"-"`
	Revision    string `gorm:"size:64;not null" json:"revision"`
}
