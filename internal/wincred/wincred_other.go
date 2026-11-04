//go:build !windows

package wincred

import "errors"

var ErrNotFound = errors.New("wincred: нет записи")

var errNoStore = errors.New("wincred: диспетчер учётных данных есть только в Windows")

func Set(target, secret string) error   { return errNoStore }
func Get(target string) (string, error) { return "", errNoStore }
func Delete(target string) error        { return errNoStore }
