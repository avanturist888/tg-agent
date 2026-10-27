package maxc

import (
	"encoding/binary"
	"errors"
)

// wrappedExt — расширение msgpack, в котором сервер прячет обычное значение
// (внутри — ещё один msgpack; так приходят, например, числа в ответе на
// LOGIN). PyMax разворачивает его в ext_hook.
const wrappedExt = 1

const maxDepth = 64

var errTruncated = errors.New("msgpack: обрезанное значение")

// unwrapExt — тот же msgpack, но обёрнутые значения подставлены на место
// расширений: дальше его разбирают обычные структуры.
func unwrapExt(src []byte) ([]byte, error) {
	if !hasExt(src) {
		return src, nil
	}
	dst := make([]byte, 0, len(src))
	pos := 0
	for pos < len(src) {
		var err error
		if pos, err = walk(src, pos, &dst, 0); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// hasExt — есть ли хоть один байт, похожий на заголовок расширения (иначе
// разбирать незачем; ложные срабатывания в строках просто дают лишний проход).
func hasExt(b []byte) bool {
	for _, c := range b {
		if (c >= 0xc7 && c <= 0xc9) || (c >= 0xd4 && c <= 0xd8) {
			return true
		}
	}
	return false
}

// walk — скопировать одно значение из src[pos:] в dst; вернуть позицию за ним.
func walk(src []byte, pos int, dst *[]byte, depth int) (int, error) {
	if depth > maxDepth {
		return 0, errors.New("msgpack: слишком глубокая вложенность")
	}
	if pos >= len(src) {
		return 0, errTruncated
	}
	c := src[pos]
	need := func(n int) error {
		if pos+n > len(src) {
			return errTruncated
		}
		return nil
	}
	be := func(off, size int) int {
		switch size {
		case 1:
			return int(src[pos+off])
		case 2:
			return int(binary.BigEndian.Uint16(src[pos+off:]))
		default:
			return int(binary.BigEndian.Uint32(src[pos+off:]))
		}
	}
	scalar := func(n int) (int, error) { // байт кода + n байт
		if err := need(1 + n); err != nil {
			return 0, err
		}
		*dst = append(*dst, src[pos:pos+1+n]...)
		return pos + 1 + n, nil
	}
	sized := func(lenSize int) (int, error) { // код, длина, данные (str, bin)
		if err := need(1 + lenSize); err != nil {
			return 0, err
		}
		return scalar(lenSize + be(1, lenSize))
	}
	container := func(head, n int) (int, error) { // заголовок, потом n значений
		if err := need(head); err != nil {
			return 0, err
		}
		*dst = append(*dst, src[pos:pos+head]...)
		p := pos + head
		for range n {
			var err error
			if p, err = walk(src, p, dst, depth+1); err != nil {
				return 0, err
			}
		}
		return p, nil
	}
	ext := func(head, n int) (int, error) { // код, [длина], тип, данные
		if err := need(head + n); err != nil {
			return 0, err
		}
		typ := int8(src[pos+head-1])
		data := src[pos+head : pos+head+n]
		if typ != wrappedExt { // незнакомое — nil: одно поле не стоит всего ответа
			*dst = append(*dst, 0xc0)
			return pos + head + n, nil
		}
		for p := 0; p < len(data); { // обычно одно значение
			var err error
			if p, err = walk(data, p, dst, depth+1); err != nil {
				return 0, err
			}
		}
		return pos + head + n, nil
	}

	switch {
	case c <= 0x7f || c >= 0xe0, c == 0xc0, c == 0xc2, c == 0xc3:
		return scalar(0)
	case c&0xf0 == 0x80: // fixmap
		return container(1, 2*int(c&0x0f))
	case c&0xf0 == 0x90: // fixarray
		return container(1, int(c&0x0f))
	case c&0xe0 == 0xa0: // fixstr
		return scalar(int(c & 0x1f))
	}
	switch c {
	case 0xc4, 0xd9: // bin8, str8
		return sized(1)
	case 0xc5, 0xda:
		return sized(2)
	case 0xc6, 0xdb:
		return sized(4)
	case 0xc7, 0xc8, 0xc9: // ext8/16/32
		size := map[byte]int{0xc7: 1, 0xc8: 2, 0xc9: 4}[c]
		if err := need(1 + size); err != nil {
			return 0, err
		}
		return ext(1+size+1, be(1, size))
	case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8: // fixext 1/2/4/8/16
		return ext(2, 1<<(c-0xd4))
	case 0xca, 0xce, 0xd2: // float32, uint32, int32
		return scalar(4)
	case 0xcb, 0xcf, 0xd3:
		return scalar(8)
	case 0xcc, 0xd0:
		return scalar(1)
	case 0xcd, 0xd1:
		return scalar(2)
	case 0xdc: // array16
		if err := need(3); err != nil {
			return 0, err
		}
		return container(3, be(1, 2))
	case 0xdd:
		if err := need(5); err != nil {
			return 0, err
		}
		return container(5, be(1, 4))
	case 0xde: // map16
		if err := need(3); err != nil {
			return 0, err
		}
		return container(3, 2*be(1, 2))
	case 0xdf:
		if err := need(5); err != nil {
			return 0, err
		}
		return container(5, 2*be(1, 4))
	}
	return 0, errors.New("msgpack: неизвестный код")
}
