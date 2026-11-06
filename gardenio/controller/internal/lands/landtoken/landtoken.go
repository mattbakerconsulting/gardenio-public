// Package landtoken mints a tenant land's coopmux bearer (gd-ApgYe740ps).
//
// coopmux verifies it (coop/crates/mux/src/transport/land_principal.rs,
// gd-zVTBvaSIdG) and learns the land from the token, never from a
// registration body. The token is
//
//	coopmux-land.v1.<land>.<base64url, no padding: HMAC-SHA256(key, "coopmux-land.v1.<land>")>
//
// under a key only coopmux and the controller hold, named on both by
// COOP_MUX_LAND_TOKEN_KEY_FILE. A land token is scoped to its own land's
// sessions and cannot reach coopmux's credential endpoints, so a tenant pod
// carries it in place of the fleet's static coopmux token.
//
// MintNATS makes the land's NATS password under the same key, with its own
// prefix ("nats-land.v1."), so neither token is ever the other. The
// controller's auth callout verifies it (VerifyNATS, internal/lands/natsland,
// gd-sS7bSXOdGs). A NATS password also carries the land's epoch, so bumping
// one land's epoch revokes that land's password alone, without rotating the
// key every land's tokens share (gd-zo4W7Q8Yno).
package landtoken

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// EnvKeyFile names the file holding the HMAC key, the same variable coopmux
// reads.
const EnvKeyFile = "COOP_MUX_LAND_TOKEN_KEY_FILE"

const (
	prefix     = "coopmux-land.v1."
	natsPrefix = "nats-land.v1."
	maxLandLen = 58
)

// ErrNotALand is returned for a name coopmux would not accept as a land.
var ErrNotALand = errors.New("not a land name")

// LoadKey reads the key file, trimming trailing whitespace as coopmux does.
// An empty key is an error: it would make every land's token public.
func LoadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", EnvKeyFile, path, err)
	}
	key := bytes.TrimSpace(raw)
	if len(key) == 0 {
		return nil, fmt.Errorf("%s %s is empty", EnvKeyFile, path)
	}
	return key, nil
}

// ErrBadToken is returned by VerifyNATS for a token that is not the land's
// NATS token at its epoch under the key.
var ErrBadToken = errors.New("not a valid land NATS token")

// Mint returns land's coopmux token under key.
func Mint(key []byte, land string) (string, error) {
	return mint(key, prefix, land, "")
}

// MintNATS returns land's NATS password at epoch under key:
//
//	nats-land.v1.<land>[.<epoch>].<base64url HMAC-SHA256(key, all before it)>
//
// Epoch 0 leaves the epoch out, so it is the password minted before epochs
// existed.
func MintNATS(key []byte, land string, epoch uint64) (string, error) {
	var suffix string
	if epoch != 0 {
		suffix = "." + strconv.FormatUint(epoch, 10)
	}
	return mint(key, natsPrefix, land, suffix)
}

// VerifyNATS returns nil when token is land's NATS password at epoch under
// key, and ErrBadToken otherwise. A password minted at any other epoch, or
// for another land, is refused.
func VerifyNATS(key []byte, token, land string, epoch uint64) error {
	want, err := MintNATS(key, land, epoch)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(token), []byte(want)) {
		return ErrBadToken
	}
	return nil
}

func mint(key []byte, prefix, land, suffix string) (string, error) {
	if len(key) == 0 {
		return "", errors.New("empty land token key")
	}
	if !validLand(land) {
		return "", fmt.Errorf("%w: %q", ErrNotALand, land)
	}
	msg := prefix + land + suffix
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// validLand is coopmux's valid_land: [a-z0-9-]{1,58}.
func validLand(land string) bool {
	if land == "" || len(land) > maxLandLen {
		return false
	}
	for _, b := range []byte(land) {
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' {
			return false
		}
	}
	return true
}
