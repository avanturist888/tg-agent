// Package wincred — секреты в диспетчере учётных данных Windows (generic
// credentials, как у ~/nestor/tools/cred.ps1: значение — UTF-16). Так
// секрет не лежит файлом: не уходит в бэкап и копию папки, его не найдёт
// grep по data/.
package wincred

import (
	"errors"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	advapi    = syscall.NewLazyDLL("advapi32.dll")
	credWrite = advapi.NewProc("CredWriteW")
	credRead  = advapi.NewProc("CredReadW")
	credDel   = advapi.NewProc("CredDeleteW")
	credFree  = advapi.NewProc("CredFree")
)

const (
	typeGeneric  = 1
	persistLocal = 2
	errNotFound  = 1168
)

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// ErrNotFound — такой записи нет.
var ErrNotFound = errors.New("wincred: нет записи")

// Set — записать секрет под именем target.
func Set(target, secret string) error {
	t, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	user, _ := syscall.UTF16PtrFromString("api")
	blob := utf16.Encode([]rune(secret))
	if len(blob) == 0 {
		return errors.New("wincred: пустой секрет")
	}
	c := credential{Type: typeGeneric, TargetName: t, UserName: user, Persist: persistLocal,
		CredentialBlobSize: uint32(len(blob) * 2), CredentialBlob: (*byte)(unsafe.Pointer(&blob[0]))}
	r, _, e := credWrite.Call(uintptr(unsafe.Pointer(&c)), 0)
	if r == 0 {
		return e
	}
	return nil
}

// Get — секрет по имени (ErrNotFound — нет записи).
func Get(target string) (string, error) {
	t, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return "", err
	}
	var p *credential
	r, _, e := credRead.Call(uintptr(unsafe.Pointer(t)), typeGeneric, 0, uintptr(unsafe.Pointer(&p)))
	if r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errNotFound {
			return "", ErrNotFound
		}
		return "", e
	}
	defer credFree.Call(uintptr(unsafe.Pointer(p)))
	n := int(p.CredentialBlobSize / 2)
	if n == 0 {
		return "", nil
	}
	words := unsafe.Slice((*uint16)(unsafe.Pointer(p.CredentialBlob)), n)
	return string(utf16.Decode(words)), nil
}

// Delete — убрать запись (нет её — не ошибка).
func Delete(target string) error {
	t, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r, _, e := credDel.Call(uintptr(unsafe.Pointer(t)), typeGeneric, 0)
	if r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errNotFound {
			return nil
		}
		return e
	}
	return nil
}
