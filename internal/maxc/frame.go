// Package maxc — клиент внутреннего протокола мессенджера MAX (max.ru) в
// режиме веб-клиента: WebSocket wss://api.oneme.ru/websocket, вход по QR.
// Протокол сверен с PyMax (github.com/MaxApiTeam/PyMax) и живыми пробами.
//
// Кадр одинаков для TCP и WebSocket: заголовок `>BBHHI` — версия (10), cmd,
// seq, opcode и слово «flags<<24 | длина», за ним тело в msgpack. Сервер
// жмёт тело: flags 0xFF — zstd, 0x01–0xFE — блок LZ4; свои кадры шлём без
// сжатия.
package maxc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	protoVersion = 10
	headerSize   = 10
	maxPayload   = 1<<24 - 1
	maxUnpacked  = 16 << 20 // распакованное тело больше этого — отказ
)

// Тип кадра.
const (
	CmdRequest  byte = 0 // наш запрос или событие от сервера
	CmdResponse byte = 1
	CmdEvent    byte = 2
	CmdError    byte = 3
)

// Frame — разобранный кадр; Payload — тело msgpack, уже распакованное.
type Frame struct {
	Cmd     byte
	Seq     uint16
	Opcode  uint16
	Payload []byte
}

// Decode — тело кадра в v (теги json: одна структура и для msgpack, и для
// ответа агенту).
func (f Frame) Decode(v any) error {
	if len(f.Payload) == 0 {
		return nil
	}
	return unmarshal(f.Payload, v)
}

// encodeFrame — кадр с телом payload (nil — пустое тело).
func encodeFrame(cmd byte, seq, opcode uint16, payload any) ([]byte, error) {
	var body []byte
	if payload != nil {
		var err error
		if body, err = marshal(payload); err != nil {
			return nil, err
		}
	}
	if len(body) > maxPayload {
		return nil, fmt.Errorf("maxc: тело кадра %d байт — больше предела", len(body))
	}
	out := make([]byte, headerSize, headerSize+len(body))
	out[0], out[1] = protoVersion, cmd
	binary.BigEndian.PutUint16(out[2:], seq)
	binary.BigEndian.PutUint16(out[4:], opcode)
	binary.BigEndian.PutUint32(out[6:], uint32(len(body)))
	return append(out, body...), nil
}

// decodeFrames — все кадры из сообщения WebSocket (обычно один).
func decodeFrames(data []byte) ([]Frame, error) {
	var frames []Frame
	for len(data) > 0 {
		if len(data) < headerSize {
			return frames, errors.New("maxc: обрезанный заголовок кадра")
		}
		word := binary.BigEndian.Uint32(data[6:])
		flags, n := byte(word>>24), int(word&maxPayload)
		if len(data) < headerSize+n {
			return frames, fmt.Errorf("maxc: обрезанное тело кадра: %d из %d байт", len(data)-headerSize, n)
		}
		f := Frame{Cmd: data[1], Seq: binary.BigEndian.Uint16(data[2:]), Opcode: binary.BigEndian.Uint16(data[4:])}
		body := data[headerSize : headerSize+n]
		var err error
		switch {
		case n == 0:
		case flags == 0xFF:
			body, err = unzstd(body)
		case flags > 0:
			body, err = unlz4(body, maxUnpacked)
		}
		if err == nil {
			body, err = unwrapExt(body)
		}
		if err != nil {
			return frames, fmt.Errorf("maxc: кадр opcode %d: %w", f.Opcode, err)
		}
		f.Payload = body
		frames = append(frames, f)
		data = data[headerSize+n:]
	}
	return frames, nil
}

var zstdDec, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxUnpacked), zstd.WithDecoderConcurrency(1))

func unzstd(src []byte) ([]byte, error) { return zstdDec.DecodeAll(src, nil) }

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.SetCustomStructTag("json")
	enc.SetOmitEmpty(false)
	enc.UseCompactInts(true)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unmarshal(data []byte, v any) error {
	dec := msgpack.NewDecoder(bytes.NewReader(data))
	dec.SetCustomStructTag("json")
	// ключи-числа (participants: {id: время}) — строками, как в JSON
	dec.SetMapDecoder(decodeLooseMap)
	return dec.Decode(v)
}

// decodeLooseMap — словарь с любыми ключами → map[string]any.
func decodeLooseMap(d *msgpack.Decoder) (any, error) {
	n, err := d.DecodeMapLen()
	if err != nil || n < 0 {
		return nil, err
	}
	m := make(map[string]any, n)
	for range n {
		k, err := d.DecodeInterfaceLoose()
		if err != nil {
			return nil, err
		}
		v, err := d.DecodeInterfaceLoose()
		if err != nil {
			return nil, err
		}
		switch k := k.(type) {
		case string:
			m[k] = v
		case []byte:
			m[string(k)] = v
		default:
			m[fmt.Sprint(k)] = v
		}
	}
	return m, nil
}
