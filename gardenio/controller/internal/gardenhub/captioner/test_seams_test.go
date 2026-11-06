package captioner

import (
	"encoding/json"
	"time"
)

// WithClock sets the clock the cache, the rate cap and the daily ceiling read.
func WithClock(now func() time.Time) Option { return func(k *Captioner) { k.now = now } }

// CommandOf is the command of a Bash tool_input, and nothing else from it: the only field Haiku is ever sent.
func CommandOf(toolInput json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(toolInput, &in) != nil {
		return ""
	}
	return in.Command
}

func (c *lru) len() int { return c.order.Len() }
