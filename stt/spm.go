package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	spNormal      = 1
	spUnknown     = 2
	spControl     = 3
	spUserDefined = 4
	spUnused      = 5
	spByte        = 6
)

type spPiece struct {
	piece string
	typ   int
}

// spDecoder — декодер SentencePiece ModelProto (v4_rnnt_e2e_tokenizer.model).
type spDecoder struct {
	pieces []spPiece
}

func loadSentencePiece(path string) (*spDecoder, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("прочитать токенайзер %s: %w", path, err)
	}
	d, err := parseSentencePiece(raw)
	if err != nil {
		return nil, fmt.Errorf("разобрать токенайзер %s: %w", path, err)
	}
	return d, nil
}

// parseSentencePiece разбирает protobuf sentencepiece.ModelProto:
// field 1 (length-delimited) = repeated SentencePiece.
func parseSentencePiece(b []byte) (*spDecoder, error) {
	var pieces []spPiece
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("повреждённый тег protobuf в ModelProto")
		}
		b = b[n:]
		fieldNum := tag >> 3
		wireType := tag & 7
		if fieldNum == 1 && wireType == 2 {
			l, ln := binary.Uvarint(b)
			if ln <= 0 || l > uint64(len(b[ln:])) {
				return nil, errors.New("повреждённая длина SentencePiece")
			}
			entry := b[ln : ln+int(l)]
			b = b[ln+int(l):]
			sp, err := parseSentencePieceEntry(entry)
			if err != nil {
				return nil, err
			}
			pieces = append(pieces, sp)
		} else {
			var err error
			b, err = skipProtoField(b, wireType)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(pieces) == 0 {
		return nil, errors.New("в ModelProto не найдено ни одного токена")
	}
	return &spDecoder{pieces: pieces}, nil
}

func parseSentencePieceEntry(b []byte) (spPiece, error) {
	sp := spPiece{typ: spNormal}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return spPiece{}, errors.New("повреждённый тег protobuf в SentencePiece")
		}
		b = b[n:]
		fieldNum := tag >> 3
		wireType := tag & 7
		switch {
		case fieldNum == 1 && wireType == 2:
			l, ln := binary.Uvarint(b)
			if ln <= 0 || l > uint64(len(b[ln:])) {
				return spPiece{}, errors.New("повреждённая длина piece")
			}
			sp.piece = string(b[ln : ln+int(l)])
			b = b[ln+int(l):]
		case fieldNum == 3 && wireType == 0:
			v, vn := binary.Uvarint(b)
			if vn <= 0 {
				return spPiece{}, errors.New("повреждённый type в SentencePiece")
			}
			sp.typ = int(v)
			b = b[vn:]
		default:
			var err error
			b, err = skipProtoField(b, wireType)
			if err != nil {
				return spPiece{}, err
			}
		}
	}
	return sp, nil
}

func skipProtoField(b []byte, wireType uint64) ([]byte, error) {
	switch wireType {
	case 0: // varint
		_, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("повреждённый varint в protobuf")
		}
		return b[n:], nil
	case 1: // 64-bit
		if len(b) < 8 {
			return nil, errors.New("нехватка байт для 64-bit поля protobuf")
		}
		return b[8:], nil
	case 2: // length-delimited
		l, n := binary.Uvarint(b)
		if n <= 0 || l > uint64(len(b[n:])) {
			return nil, errors.New("нехватка байт для length-delimited поля protobuf")
		}
		return b[n+int(l):], nil
	case 5: // 32-bit
		if len(b) < 4 {
			return nil, errors.New("нехватка байт для 32-bit поля protobuf")
		}
		return b[4:], nil
	default:
		return nil, fmt.Errorf("неподдерживаемый wire type protobuf: %d", wireType)
	}
}

// decode собирает строку из последовательности ID токенов.
func (d *spDecoder) decode(ids []int64) string {
	var buf []byte
	for _, id := range ids {
		if id < 0 || int(id) >= len(d.pieces) {
			continue
		}
		p := d.pieces[id]
		if p.typ == spControl || p.typ == spUnused || p.typ == spUnknown {
			continue
		}
		if p.typ == spByte {
			if bv, ok := parseSPByte(p.piece); ok {
				buf = append(buf, bv)
				continue
			}
		}
		buf = append(buf, p.piece...)
	}
	return strings.TrimSpace(strings.ReplaceAll(string(buf), "\u2581", " "))
}

func parseSPByte(s string) (byte, bool) {
	if len(s) != 6 || s[0] != '<' || s[1] != '0' || (s[2] != 'x' && s[2] != 'X') || s[5] != '>' {
		return 0, false
	}
	hi, ok1 := fromHex(s[3])
	lo, ok2 := fromHex(s[4])
	if !ok1 || !ok2 {
		return 0, false
	}
	return (hi << 4) | lo, true
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
