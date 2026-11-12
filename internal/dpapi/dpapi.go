// Package dpapi — секрет, который можно хранить рядом с данными: на Windows
// он зашифрован DPAPI под текущего пользователя (CryptProtectData), и
// расшифровать его может только этот пользователь на этом компьютере. Копия
// базы, отданная для разбора, секрета не раскрывает.
//
// Seal одного и того же значения в процессе даёт одну и ту же строку (кэш):
// иначе каждое сохранение меняло бы запись, в которой ничего не поменялось.
package dpapi

import (
	"encoding/base64"
	"sync"
)

// Prefix — метка зашифрованного значения (без неё — открытый текст старой
// записи).
const Prefix = "dpapi:"

var (
	mu     sync.Mutex
	sealed = map[string]string{} // открытое → зашифрованное
	opened = map[string]string{} // зашифрованное → открытое
)

// Seal — зашифровать (пустое — пустое). Где DPAPI нет, — как есть: тогда
// безопасность держится на правах к каталогу данных, как раньше.
func Seal(plain string) (string, error) {
	if plain == "" || !Available {
		return plain, nil
	}
	mu.Lock()
	defer mu.Unlock()
	if s, ok := sealed[plain]; ok {
		return s, nil
	}
	b, err := protect([]byte(plain))
	if err != nil {
		return "", err
	}
	s := Prefix + base64.StdEncoding.EncodeToString(b)
	sealed[plain], opened[s] = s, plain
	return s, nil
}

// Open — расшифровать то, что дал Seal; значение без метки — открытый текст
// старой записи, отдаётся как есть.
func Open(v string) (string, error) {
	if len(v) < len(Prefix) || v[:len(Prefix)] != Prefix {
		return v, nil
	}
	mu.Lock()
	defer mu.Unlock()
	if p, ok := opened[v]; ok {
		return p, nil
	}
	raw, err := base64.StdEncoding.DecodeString(v[len(Prefix):])
	if err != nil {
		return "", err
	}
	b, err := unprotect(raw)
	if err != nil {
		return "", err
	}
	p := string(b)
	sealed[p], opened[v] = v, p
	return p, nil
}
