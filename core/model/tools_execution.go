package model

// RECONSTRUCTED: isolated internal Tools ledger. Never migrate it automatically
// over an existing gateway and never import historical native/wallet rows.
type ToolsExecution struct {
	ID              string `gorm:"primaryKey;size:186"`
	UserID          string `gorm:"index;size:200"`
	AgentID         string `gorm:"size:200"`
	SessionID       string `gorm:"size:200"`
	Model           string `gorm:"size:256"`
	RequestHash     string `gorm:"size:64"`
	Revision        string `gorm:"size:64"`
	BindingJSON     string `gorm:"type:text;not null"`
	Status          string `gorm:"size:40"`
	UpstreamID      string `gorm:"size:200"`
	ErrorCode       string `gorm:"size:100"`
	NativeOutput    string `gorm:"type:text"`
	Output          string `gorm:"type:text"`
	Manifest        string `gorm:"type:text"`
	CancelAttempted bool
	LeaseOwner      string `gorm:"size:64"`
	LeaseUntil      int64
	CreatedAt       int64
	UpdatedAt       int64
}
type ToolsRequestNonce struct {
	Nonce     string `gorm:"primaryKey;size:64"`
	ExpiresAt int64  `gorm:"index"`
}
