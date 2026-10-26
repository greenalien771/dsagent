//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// The API key is sealed with DPAPI (CryptProtectData) in the caller's user
// context. Ciphertext is bound to this Windows account: another user, another
// machine, or a copied file gains nothing, and no password has to be stored
// next to the blob. CRYPTPROTECT_UI_FORBIDDEN keeps a background service from
// ever popping a UI prompt.

const cryptProtectUIForbidden = 0x1

var (
	procCryptProtectData   = syscall.NewLazyDLL("crypt32.dll").NewProc("CryptProtectData")
	procCryptUnprotectData = syscall.NewLazyDLL("crypt32.dll").NewProc("CryptUnprotectData")
	procLocalFree          = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
)

// entropy binds the blob to this application: another program running as the
// same user cannot simply call CryptUnprotectData on our file.
var secretEntropy = []byte("dsagent/api_key/v1")

type dataBlob struct {
	size uint32
	data *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{size: uint32(len(b)), data: &b[0]}
}

func (b dataBlob) bytes() []byte {
	if b.size == 0 || b.data == nil {
		return nil
	}
	out := make([]byte, b.size)
	copy(out, unsafe.Slice(b.data, int(b.size)))
	return out
}

// sealSecret encrypts plain with the OS keystore.
func sealSecret(plain string) ([]byte, error) {
	if plain == "" {
		return nil, errors.New("refusing to seal an empty secret")
	}
	in := newBlob([]byte(plain))
	ent := newBlob(secretEntropy)
	var out dataBlob

	r, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description
		uintptr(unsafe.Pointer(&ent)),
		0, // reserved
		0, // prompt struct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, callError("CryptProtectData", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return out.bytes(), nil
}

// openSecret decrypts a blob produced by sealSecret for this user/machine.
func openSecret(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", errors.New("empty secret")
	}
	in := newBlob(sealed)
	ent := newBlob(secretEntropy)
	var out dataBlob

	r, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description out
		uintptr(unsafe.Pointer(&ent)),
		0, // reserved
		0, // prompt struct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", callError("CryptUnprotectData", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return string(out.bytes()), nil
}

func callError(name string, err error) error {
	if err == nil || err == syscall.Errno(0) {
		return errors.New(name + " failed")
	}
	return errors.New(name + " failed: " + err.Error())
}
