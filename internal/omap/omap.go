// Package omap — JSON-объект с сохранённым порядком ключей.
//
// Агенты и человек читают ответы глазами, и поля должны идти в осмысленном
// порядке (id, date, from, text…). Обычный map в Go сортирует ключи по
// алфавиту, поэтому ответы собираем через этот тип.
package omap

import (
	"bytes"
	"encoding/json"
)

type entry struct {
	key   string
	value any
}

// Map — упорядоченный набор ключ → значение.
type Map struct{ items []entry }

func New() *Map { return &Map{} }

// Set добавляет ключ или заменяет значение, сохраняя исходную позицию.
func (m *Map) Set(key string, value any) *Map {
	for i := range m.items {
		if m.items[i].key == key {
			m.items[i].value = value
			return m
		}
	}
	m.items = append(m.items, entry{key, value})
	return m
}

func (m *Map) Get(key string) (any, bool) {
	for _, e := range m.items {
		if e.key == key {
			return e.value, true
		}
	}
	return nil, false
}

func (m *Map) Delete(key string) {
	for i, e := range m.items {
		if e.key == key {
			m.items = append(m.items[:i], m.items[i+1:]...)
			return
		}
	}
}

func (m *Map) Len() int { return len(m.items) }

// Merge дописывает ключи другого объекта.
func (m *Map) Merge(other *Map) *Map {
	if other != nil {
		for _, e := range other.items {
			m.Set(e.key, e.value)
		}
	}
	return m
}

func (m *Map) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range m.items {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(e.key)
		buf.Write(k)
		buf.WriteByte(':')
		v, err := Marshal(e.value)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Marshal — json без экранирования <, >, & и с кириллицей как есть.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Pretty — с отступом в 2 пробела, как json.dumps(indent=2).
func Pretty(v any) string {
	raw, err := Marshal(v)
	if err != nil {
		return `{"error": "json", "message": ` + quote(err.Error()) + `}`
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
