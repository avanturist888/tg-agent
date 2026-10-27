package maxc

import "errors"

// unlz4 — распаковка блока LZ4 (без заголовка кадра LZ4: сервер MAX шлёт
// голый блок, размер результата заранее не известен).
func unlz4(src []byte, limit int) ([]byte, error) {
	dst := make([]byte, 0, min(len(src)*4, limit))
	for pos := 0; pos < len(src); {
		token := src[pos]
		pos++
		lit := int(token >> 4)
		if lit == 15 {
			for {
				if pos >= len(src) {
					return nil, errors.New("lz4: обрезана длина литералов")
				}
				b := src[pos]
				pos++
				lit += int(b)
				if b != 255 {
					break
				}
			}
		}
		if pos+lit > len(src) {
			return nil, errors.New("lz4: литералы за концом блока")
		}
		if len(dst)+lit > limit {
			return nil, errors.New("lz4: распакованное больше предела")
		}
		dst = append(dst, src[pos:pos+lit]...)
		pos += lit
		if pos == len(src) {
			break // последний кусок — одни литералы
		}
		if pos+2 > len(src) {
			return nil, errors.New("lz4: обрезано смещение")
		}
		offset := int(src[pos]) | int(src[pos+1])<<8
		pos += 2
		if offset == 0 || offset > len(dst) {
			return nil, errors.New("lz4: смещение за началом")
		}
		match := int(token&0x0F) + 4
		if token&0x0F == 0x0F {
			for {
				if pos >= len(src) {
					return nil, errors.New("lz4: обрезана длина совпадения")
				}
				b := src[pos]
				pos++
				match += int(b)
				if b != 255 {
					break
				}
			}
		}
		if len(dst)+match > limit {
			return nil, errors.New("lz4: распакованное больше предела")
		}
		start := len(dst) - offset
		for i := range match { // совпадение может перекрывать само себя
			dst = append(dst, dst[start+i])
		}
	}
	return dst, nil
}
