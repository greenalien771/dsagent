//go:build !windows

package main

import (
	"encoding/base64"
	"errors"
	"strings"
)

// Non-Windows fallback.
//
// There is no OS keystore we can reach without a cgo dependency here, so the
// key is stored base64-encoded in a 0600 file inside the user's config dir. That
// is obfuscation, not encryption: it stops a casual `cat`, not a local attacker.
// The honest answer on macOS/Linux is to put the key in DEEPSEEK_API_KEY and
// keep nothing on disk, which the UI offers.
const plainPrefix = "b64:v1:"

func sealSecret(plain string) ([]byte, error) {
	if plain == "" {
		return nil, errors.New("refusing to store an empty secret")
	}
	return []byte(plainPrefix + base64.StdEncoding.EncodeToString([]byte(plain))), nil
}

func openSecret(sealed []byte) (string, error) {
	s := string(sealed)
	if !strings.HasPrefix(s, plainPrefix) {
		return "", errors.New("unrecognised secret format")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, plainPrefix))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
