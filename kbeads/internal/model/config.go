package model

import (
	"encoding/json"
	"time"
)

// Config is a key-value configuration record stored as JSONB.
// Keys use the format "{namespace}:{name}" (e.g. "type:decision", "view:inbox", "context:prime").
type Config struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}
