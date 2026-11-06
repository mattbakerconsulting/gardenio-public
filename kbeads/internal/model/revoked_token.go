package model

import "time"

// RevokedToken represents a persisted entry in the revoked_tokens table. The
// token itself is never stored — only its SHA-256 hash (64-char hex).
type RevokedToken struct {
	TokenHash string
	RevokedAt time.Time
	RevokedBy string
	Reason    string
}
