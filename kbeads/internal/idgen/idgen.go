// Package idgen provides short, URL-safe unique ID generation backed by nanoid.
package idgen

import (
	"fmt"

	nanoid "github.com/matoous/go-nanoid/v2"
)

// DefaultPrefix is prepended to every generated ID.
var DefaultPrefix = "gd-"

// Alphabet defines the character set used for the random portion of the ID.
var Alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Length is the number of random characters generated (excluding the prefix).
var Length = 10

// Generate returns a new unique ID using the default prefix.
func Generate() (string, error) {
	return GenerateWithPrefix(DefaultPrefix)
}

// GenerateWithPrefix returns a new unique ID with the given prefix.
func GenerateWithPrefix(prefix string) (string, error) {
	id, err := nanoid.Generate(Alphabet, Length)
	if err != nil {
		return "", fmt.Errorf("idgen: %w", err)
	}
	return prefix + id, nil
}
