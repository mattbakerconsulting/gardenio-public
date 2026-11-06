package model

import "time"

// Jack constraint constants.
const (
	JackMaxTTL             = 7 * 24 * time.Hour // Maximum initial TTL
	JackMaxSingleExtension = 24 * time.Hour     // Maximum per-extension TTL
	JackMaxExtensions      = 5                  // Maximum number of extensions
	JackMaxCumulativeTTL   = 7 * 24 * time.Hour // Maximum cumulative TTL
	JackMaxChanges         = 500                // Maximum change records per jack
	JackMaxChangeFieldSize = 5 * 1024           // Maximum bytes per before/after field
)

// Jack label constants.
const (
	JackLabelPrefix     = "jack:"
	LabelJackDebug      = "jack:debug"
	LabelJackHotfix     = "jack:hotfix"
	LabelJackFailover   = "jack:failover"
	LabelJackConfig     = "jack:config"
	LabelJackExperiment = "jack:experiment"
	LabelJackGeneral    = "jack:general"
)

// JackChange represents a single recorded change within a jack.
type JackChange struct {
	Timestamp string `json:"timestamp"`
	Action    string `json:"action"`              // edit|exec|patch|delete|create
	Target    string `json:"target,omitempty"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
	Cmd       string `json:"cmd,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"` // true if secrets detected and redacted
}

// ValidJackActions lists the allowed actions for jack log entries.
var ValidJackActions = []string{"edit", "exec", "patch", "delete", "create"}

// IsValidJackAction checks if an action string is valid.
func IsValidJackAction(action string) bool {
	for _, a := range ValidJackActions {
		if a == action {
			return true
		}
	}
	return false
}
