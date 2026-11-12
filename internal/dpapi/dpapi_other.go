//go:build !windows

package dpapi

import "errors"

// Available — шифрования нет: Seal отдаёт как есть.
const Available = false

func protect([]byte) ([]byte, error)   { return nil, errors.New("dpapi: только Windows") }
func unprotect([]byte) ([]byte, error) { return nil, errors.New("dpapi: только Windows") }
