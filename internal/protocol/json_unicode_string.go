package protocol

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// decodeUnicodeString rejects invalid Unicode before decoding a wire string.
// json.Unmarshal alone replaces unpaired UTF-16 surrogates.
func decodeUnicodeString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	var value string
	if !utf8.Valid(raw) || isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return "", false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return "", false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return "", false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return "", false
			}
			i += 6
		}
	}
	return value, true
}
