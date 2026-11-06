package model

import "time"

// RunnerToken is one issued runner bearer (local-agents spec §4.2). Only the
// SHA-256 hash is stored; the token itself is returned once at registration.
type RunnerToken struct {
	TokenHash string
	RunnerID  string
	CreatedAt time.Time
	RevokedAt *time.Time
}
