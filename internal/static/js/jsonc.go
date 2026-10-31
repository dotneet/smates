package js

import (
	"bytes"
	"encoding/json"
)

// parseJSONC decodes JSON with comments and trailing commas (the dialect of
// tsconfig.json) into v.
func parseJSONC(b []byte, v any) error {
	return json.Unmarshal(stripJSONC(b), v)
}

// stripJSONC rewrites JSONC to plain JSON: it drops a UTF-8 BOM, // and /* */ comments,
// and commas directly before a closing '}' or ']'. String contents are kept verbatim.
func stripJSONC(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(b) && b[j] != '"' {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j, len(b)-1)
			out = append(out, b[i:j+1]...)
			i = j
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i+1 < len(b) && b[i+1] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			end := bytes.Index(b[i+2:], []byte("*/"))
			if end < 0 {
				i = len(b)
			} else {
				i += end + 3
			}
			out = append(out, ' ')
		case c == '}' || c == ']':
			k := len(out) - 1
			for k >= 0 && isSpace(out[k]) {
				k--
			}
			if k >= 0 && out[k] == ',' {
				out[k] = ' '
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}
