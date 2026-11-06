package deathtranscript

import (
	"fmt"
	"strings"
)

// TranscriptChunkKey composes the S3 key for a numbered transcript snapshot.
// spawn may be "" for the legacy flat layout. Useful for logging / diagnostics.
func TranscriptChunkKey(prefix, sessionKey, spawn string, n int) string {
	prefix = strings.TrimRight(orDefaultPrefix(prefix), "/")
	if spawn == "" {
		return fmt.Sprintf("%s/%s/%s%d.jsonl", prefix, sessionKey, transcriptsSeg, n)
	}
	return fmt.Sprintf("%s/%s/%s/%s%d.jsonl", prefix, sessionKey, spawn, transcriptsSeg, n)
}

func orDefaultPrefix(p string) string {
	if strings.TrimSpace(p) == "" {
		return DefaultPrefix
	}
	return p
}
