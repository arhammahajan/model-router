package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"unicode/utf8"

	"github.com/arhammahajan/router/internal/protocol"
)

// canonicalHashWriter hashes expanded ensure_ascii bytes in fixed-size chunks.
// Its storage does not grow with sessionId or its escaped representation.
type canonicalHashWriter struct {
	digest hash.Hash
	block  [sha256.BlockSize]byte
	used   int
}

func (w *canonicalHashWriter) writeByte(b byte) {
	w.block[w.used] = b
	w.used++
	if w.used == len(w.block) {
		w.flush()
	}
}

func (w *canonicalHashWriter) writeString(s string) {
	for i := 0; i < len(s); i++ {
		w.writeByte(s[i])
	}
}

func (w *canonicalHashWriter) flush() {
	if w.used != 0 {
		_, _ = w.digest.Write(w.block[:w.used])
		w.used = 0
	}
}

// canonicalSessionHash parses one raw JSON string value and returns the
// agy-acp-sha256 identity over the canonical ensure_ascii bytes. empty
// reports a decoded empty string (select fallback). Malformed JSON strings
// and invalid UTF8 reject value-free.
func canonicalSessionHash(raw json.RawMessage) (id string, empty bool, rerr *protocol.RouterError) {
	bad := func() *protocol.RouterError {
		return &protocol.RouterError{Status: 400, Code: "invalid_session_id", Msg: "'sessionId' must be a string or null"}
	}
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false, bad()
	}
	if !utf8.Valid(raw) {
		return "", false, bad()
	}
	canon := canonicalHashWriter{digest: sha256.New()}
	canon.writeByte('"')
	n := 0
	i := 1
	end := len(raw) - 1
	for i < end {
		c := raw[i]
		if c == '"' {
			return "", false, bad()
		}
		if c == '\\' {
			i++
			if i >= end {
				return "", false, bad()
			}
			e := raw[i]
			switch e {
			case '"':
				canon.writeString(`\"`)
				n++
			case '\\':
				canon.writeString(`\\`)
				n++
			case '/':
				canon.writeByte('/')
				n++
			case 'b':
				canon.writeString(`\b`)
				n++
			case 'f':
				canon.writeString(`\f`)
				n++
			case 'n':
				canon.writeString(`\n`)
				n++
			case 'r':
				canon.writeString(`\r`)
				n++
			case 't':
				canon.writeString(`\t`)
				n++
			case 'u':
				if i+4 >= len(raw) {
					return "", false, bad()
				}
				hexDigits := string(raw[i+1 : i+5])
				v, ok := parseHex4(hexDigits)
				if !ok {
					return "", false, bad()
				}
				i += 4
				if v >= 0xD800 && v <= 0xDBFF {
					// Possible surrogate pair: look for a following \uXXXX low surrogate.
					if i+1 < end && raw[i+1] == '\\' && i+2 < end && raw[i+2] == 'u' {
						if i+6 < len(raw) {
							lowHex := string(raw[i+3 : i+7])
							if low, ok := parseHex4(lowHex); ok && low >= 0xDC00 && low <= 0xDFFF {
								cp := 0x10000 + (v-0xD800)*0x400 + (low - 0xDC00)
								writeEscaped(&canon, cp)
								n++
								i += 6
								break
							}
						}
					}
					writeEscaped(&canon, int(v))
					n++
				} else {
					writeEscaped(&canon, int(v))
					n++
				}
			default:
				return "", false, bad()
			}
			i++
			continue
		}
		if c < 0x20 {
			return "", false, bad()
		}
		if c < 0x80 {
			switch c {
			case '"':
				return "", false, bad()
			case '\\':
				return "", false, bad()
			default:
				if c == 0x7F {
					canon.writeString(`\u007f`)
				} else {
					canon.writeByte(c)
				}
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(raw[i:end])
		if r == utf8.RuneError && size <= 1 {
			return "", false, bad()
		}
		writeEscaped(&canon, int(r))
		n++
		i += size
	}
	canon.writeByte('"')
	if n == 0 {
		return "", true, nil
	}
	canon.flush()
	return "agy-acp-" + hex.EncodeToString(canon.digest.Sum(nil)), false, nil
}

func parseHex4(s string) (int, bool) {
	if len(s) != 4 {
		return 0, false
	}
	var v int
	for i := 0; i < 4; i++ {
		c := s[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, false
		}
		v = v*16 + d
	}
	return v, true
}

// writeEscaped appends one code point in Python json ensure_ascii style:
// short escapes, \u00xx for other controls and DEL, literals for printable
// ASCII, \uXXXX for BMP (lowercase hex, lone surrogates preserved) and
// surrogate pairs for astral planes.
func writeEscaped(buf *canonicalHashWriter, cp int) {
	switch cp {
	case 0x08:
		buf.writeString(`\b`)
		return
	case 0x09:
		buf.writeString(`\t`)
		return
	case 0x0A:
		buf.writeString(`\n`)
		return
	case 0x0C:
		buf.writeString(`\f`)
		return
	case 0x0D:
		buf.writeString(`\r`)
		return
	case 0x22:
		buf.writeString(`\"`)
		return
	case 0x5C:
		buf.writeString(`\\`)
		return
	}
	if cp < 0x20 || cp == 0x7F {
		const hexd = "0123456789abcdef"
		buf.writeString(`\u00`)
		buf.writeByte(hexd[(cp>>4)&0xF])
		buf.writeByte(hexd[cp&0xF])
		return
	}
	if cp < 0x80 {
		buf.writeByte(byte(cp))
		return
	}
	if cp < 0x10000 {
		const hexd = "0123456789abcdef"
		buf.writeString(`\u`)
		buf.writeByte(hexd[(cp>>12)&0xF])
		buf.writeByte(hexd[(cp>>8)&0xF])
		buf.writeByte(hexd[(cp>>4)&0xF])
		buf.writeByte(hexd[cp&0xF])
		return
	}
	// Astral: surrogate pair, lowercase.
	v := cp - 0x10000
	hi := 0xD800 + (v >> 10)
	lo := 0xDC00 + (v & 0x3FF)
	const hexd = "0123456789abcdef"
	buf.writeString(`\u`)
	buf.writeByte(hexd[(hi>>12)&0xF])
	buf.writeByte(hexd[(hi>>8)&0xF])
	buf.writeByte(hexd[(hi>>4)&0xF])
	buf.writeByte(hexd[hi&0xF])
	buf.writeString(`\u`)
	buf.writeByte(hexd[(lo>>12)&0xF])
	buf.writeByte(hexd[(lo>>8)&0xF])
	buf.writeByte(hexd[(lo>>4)&0xF])
	buf.writeByte(hexd[lo&0xF])
}
