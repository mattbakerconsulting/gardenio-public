package model

import "time"

// RefreshToken represents a persisted entry in the refresh_tokens table. The
// token plaintext itself is never stored — only its SHA-256 hash (64-char
// hex). ScopesJSON holds the marshalled scope set carried by the access
// tokens minted from this refresh (project, allowed_types, allowed_ops).
type RefreshToken struct {
	TokenHash         string
	Fleet             string
	Project           string
	ScopesJSON        []byte
	Subject           string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	RevokedAt         *time.Time
	ParentRefreshHash string
}
