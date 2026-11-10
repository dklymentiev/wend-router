//go:build windows

package main

import (
	"bytes"
	"errors"
	"syscall"
	"unsafe"
)

// On Windows the key file is sealed with DPAPI: only the same Windows user
// on the same machine can open it, and there is no password to keep.

var (
	crypt32          = syscall.NewLazyDLL("crypt32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procProtect      = crypt32.NewProc("CryptProtectData")
	procUnprotect    = crypt32.NewProc("CryptUnprotectData")
	procLocalFree    = kernel32.NewProc("LocalFree")
	dpapiMagic       = []byte("LLMR1D")
	cryptUIForbidden = uintptr(0x1)
)

type dataBlob struct {
	size uint32
	data *byte
}

func blob(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{size: uint32(len(b)), data: &b[0]}
}

func (b *dataBlob) bytes() []byte {
	if b.data == nil {
		return nil
	}
	return bytes.Clone(unsafe.Slice(b.data, b.size))
}

func seal(plain []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procProtect.Call(uintptr(unsafe.Pointer(blob(plain))), 0, 0, 0, 0, cryptUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return append(bytes.Clone(dpapiMagic), out.bytes()...), nil
}

func unseal(sealed []byte) ([]byte, error) {
	if !bytes.HasPrefix(sealed, dpapiMagic) {
		return nil, errors.New("not a DPAPI key file")
	}
	in := sealed[len(dpapiMagic):]
	var out dataBlob
	r, _, err := procUnprotect.Call(uintptr(unsafe.Pointer(blob(in))), 0, 0, 0, 0, cryptUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return out.bytes(), nil
}
