package customprov

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// json5ToJSON converts JSON5 text to equivalent standard JSON, so the rest of
// the config pipeline can keep using encoding/json/v2. It is a single-pass
// byte scanner: comments become same-length whitespace (newlines preserved)
// and everything else is rewritten in place.
func json5ToJSON(src []byte) ([]byte, error) {
	doc, err := convertJSON5(src)
	if err != nil {
		return nil, err
	}
	return doc.json, nil
}

// convertJSON5 converts JSON5 text and records, per token, where it came
// from, so decoding errors can be reported against the original file.
func convertJSON5(src []byte) (*json5Document, error) {
	c := &json5Converter{src: src}
	if err := c.run(); err != nil {
		return nil, err
	}
	return &json5Document{src: src, json: c.out, offsets: c.offsets}, nil
}

// json5Document is a converted document plus the offset map back to its
// JSON5 source.
type json5Document struct {
	src  []byte
	json []byte
	// offsets maps each token's start in the converted output back to its
	// start in the source; its size grows with the token count.
	offsets []offsetPair
}

// offsetPair is one out-position → source-position record.
type offsetPair struct{ out, src int }

// sourceOffset maps a converted-output byte offset back to the original
// source offset of the token containing it. It reports false when the offset
// precedes every recorded token.
func (d *json5Document) sourceOffset(outOffset int) (int, bool) {
	i := sort.Search(len(d.offsets), func(i int) bool { return d.offsets[i].out > outOffset })
	i--
	if i < 0 {
		return 0, false
	}
	return d.offsets[i].src, true
}

// lineColumn returns the 1-based line and column of a byte offset in the
// original source.
func (d *json5Document) lineColumn(pos int) (int, int) {
	return lineColumn(d.src, pos)
}

type json5Converter struct {
	src     []byte
	pos     int
	out     []byte
	offsets []offsetPair
}

func (c *json5Converter) run() error {
	for c.pos < len(c.src) {
		b := c.src[c.pos]
		switch {
		case b == '/' && c.pos+1 < len(c.src) && c.src[c.pos+1] == '/':
			if err := c.lineComment(); err != nil {
				return err
			}
		case b == '/' && c.pos+1 < len(c.src) && c.src[c.pos+1] == '*':
			if err := c.blockComment(); err != nil {
				return err
			}
		case b == '\'' || b == '"':
			if err := c.quotedString(b); err != nil {
				return err
			}
		case isIdentStart(b):
			if err := c.identifier(); err != nil {
				return err
			}
		case b == ',':
			c.mark()
			c.pos++
			// A trailing comma is dropped for a single space, so the
			// whitespace or comments after it pass through untouched.
			if next := c.peekStructural(); next == '}' || next == ']' {
				c.out = append(c.out, ' ')
			} else {
				c.out = append(c.out, ',')
			}
		case b == '+' || b == '-':
			if c.pos+1 < len(c.src) && (isDigit(c.src[c.pos+1]) || c.src[c.pos+1] == '.') {
				if err := c.number(); err != nil {
					return err
				}
			} else {
				c.mark()
				c.out = append(c.out, b)
				c.pos++
			}
		case b == '.':
			if c.pos+1 < len(c.src) && isDigit(c.src[c.pos+1]) {
				if err := c.number(); err != nil {
					return err
				}
			} else {
				c.mark()
				c.out = append(c.out, b)
				c.pos++
			}
		case isDigit(b):
			if err := c.number(); err != nil {
				return err
			}
		case b >= utf8.RuneSelf:
			// A non-ASCII rune outside strings: a Unicode letter starts an
			// unquoted key, anything else passes through.
			r, size := utf8.DecodeRune(c.src[c.pos:])
			if r == utf8.RuneError && size <= 1 {
				c.mark()
				c.out = append(c.out, b)
				c.pos++
				continue
			}
			if unicode.IsLetter(r) {
				if err := c.identifier(); err != nil {
					return err
				}
			} else {
				c.mark()
				c.out = append(c.out, c.src[c.pos:c.pos+size]...)
				c.pos += size
			}
		default:
			c.mark()
			c.out = append(c.out, b)
			c.pos++
		}
	}
	return nil
}

// mark records that the output about to be written corresponds to the
// current source position, so decoding errors can be mapped back.
func (c *json5Converter) mark() {
	c.offsets = append(c.offsets, offsetPair{out: len(c.out), src: c.pos})
}

// lineComment blanks a // comment with same-length spaces, leaving the
// newline to the main loop.
func (c *json5Converter) lineComment() error {
	c.mark()
	c.out = append(c.out, ' ', ' ')
	c.pos += 2
	for c.pos < len(c.src) && c.src[c.pos] != '\n' {
		c.out = append(c.out, ' ')
		c.pos++
	}
	return nil
}

// blockComment blanks a /* */ comment with same-length spaces, keeping the
// newlines so later errors keep the original line numbers. The opening /*
// and closing */ also become two spaces each.
func (c *json5Converter) blockComment() error {
	start := c.pos
	c.mark()
	c.out = append(c.out, ' ', ' ')
	c.pos += 2
	for c.pos < len(c.src) {
		if c.src[c.pos] == '\n' {
			c.out = append(c.out, '\n')
			c.pos++
			continue
		}
		if c.src[c.pos] == '*' && c.pos+1 < len(c.src) && c.src[c.pos+1] == '/' {
			c.out = append(c.out, ' ', ' ')
			c.pos += 2
			return nil
		}
		c.out = append(c.out, ' ')
		c.pos++
	}
	return c.errorf(start, "unterminated block comment")
}

// quotedString rewrites a quoted string as a double-quoted JSON string; both
// quote styles share JSON5's escape rules. An unescaped " gains a backslash,
// \' loses one, and JSON5-only escapes are converted (line continuations
// dropped, \x/\0/\v to \u form).
func (c *json5Converter) quotedString(quote byte) error {
	start := c.pos
	c.mark()
	c.pos++
	c.out = append(c.out, '"')
	for {
		if c.pos >= len(c.src) {
			return c.errorf(start, "unterminated string")
		}
		b := c.src[c.pos]
		switch {
		case b == quote:
			c.pos++
			c.out = append(c.out, '"')
			return nil
		case b == '"':
			// Unescaped in a single-quoted string; a double-quoted one takes
			// this as its terminator above.
			c.pos++
			c.out = append(c.out, '\\', '"')
		case b == '\\':
			if err := c.stringEscape(); err != nil {
				return err
			}
		default:
			c.pos++
			c.out = append(c.out, b)
		}
	}
}

// stringEscape rewrites one backslash escape of a quoted string. c.pos sits
// on the backslash.
func (c *json5Converter) stringEscape() error {
	if c.pos+1 >= len(c.src) {
		return c.errorf(c.pos, "unterminated string")
	}
	e := c.src[c.pos+1]
	switch e {
	case '\n':
		// Line continuation: both characters vanish.
		c.pos += 2
	case '\r':
		// CRLF line continuation: the whole terminator vanishes.
		if c.pos+2 < len(c.src) && c.src[c.pos+2] == '\n' {
			c.pos += 3
		} else {
			c.pos += 2
		}
	case '\'':
		c.pos += 2
		c.out = append(c.out, '\'')
	case 'x':
		if c.pos+3 >= len(c.src) || !isHexDigit(c.src[c.pos+2]) || !isHexDigit(c.src[c.pos+3]) {
			return c.errorf(c.pos, "invalid \\x escape: two hexadecimal digits required")
		}
		c.out = append(c.out, '\\', 'u', '0', '0', c.src[c.pos+2], c.src[c.pos+3])
		c.pos += 4
	case '0':
		if c.pos+2 < len(c.src) && isDigit(c.src[c.pos+2]) {
			// A digit after \0 is a legacy octal prefix — passed through for
			// the decoder to reject.
			c.out = append(c.out, '\\', '0')
			c.pos += 2
		} else {
			c.out = append(c.out, '\\', 'u', '0', '0', '0', '0')
			c.pos += 2
		}
	case 'v':
		c.out = append(c.out, '\\', 'u', '0', '0', '0', 'b')
		c.pos += 2
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		// JSON escapes pass through unchanged.
		c.out = append(c.out, '\\', e)
		c.pos += 2
	case 'u':
		for i := 2; i <= 5; i++ {
			if c.pos+i >= len(c.src) || !isHexDigit(c.src[c.pos+i]) {
				return c.errorf(c.pos, "invalid \\u escape: four hexadecimal digits required")
			}
		}
		c.out = append(c.out, c.src[c.pos:c.pos+6]...)
		c.pos += 6
	default:
		return c.errorf(c.pos, "unknown escape \\%s", string(rune(e)))
	}
	return nil
}

// identifier scans an ECMAScript-style IdentifierName (ASCII identifier
// characters plus Unicode letters and digits). Keys gain quotes; the value
// words true/false/null pass through; Infinity and NaN are rejected —
// standard JSON cannot represent them.
func (c *json5Converter) identifier() error {
	start := c.pos
	c.mark()
	for c.pos < len(c.src) {
		b := c.src[c.pos]
		if isIdentPart(b) {
			c.pos++
			continue
		}
		if b < utf8.RuneSelf {
			break
		}
		r, size := utf8.DecodeRune(c.src[c.pos:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		c.pos += size
	}
	word := c.src[start:c.pos]
	if next := c.peekStructural(); next == ':' {
		c.out = append(c.out, '"')
		c.out = append(c.out, word...)
		c.out = append(c.out, '"')
		return nil
	}
	switch string(word) {
	case "Infinity", "NaN":
		return c.errorf(start, "%s cannot be represented in standard JSON", string(word))
	}
	c.out = append(c.out, word...)
	return nil
}

// number rewrites one number token. Hexadecimal becomes decimal (as int64,
// overflowing errors); a leading + is dropped; a decimal point not followed
// by a digit gains a zero — after it (5.e3 → 5.0e3) or before it (.5 →
// 0.5). The negative sign applies to all of these.
func (c *json5Converter) number() error {
	start := c.pos
	c.mark()
	for c.pos < len(c.src) && isNumberPart(c.src[c.pos]) {
		c.pos++
	}
	token := string(c.src[start:c.pos])
	body := token
	if body[0] == '+' || body[0] == '-' {
		body = body[1:]
	}
	if len(body) >= 2 && body[0] == '0' && (body[1] == 'x' || body[1] == 'X') {
		// ParseInt rejects the 0x prefix when the base is given explicitly,
		// so hand it the sign and digits only.
		sign := token[:len(token)-len(body)]
		n, err := strconv.ParseInt(sign+body[2:], 16, 64)
		if err != nil {
			return c.errorf(start, "invalid hexadecimal number %q", token)
		}
		c.out = append(c.out, strconv.FormatInt(n, 10)...)
		return nil
	}
	if strings.HasPrefix(token, "+") {
		token = token[1:]
	}
	sign := ""
	if strings.HasPrefix(token, "-") {
		sign = "-"
		token = token[1:]
	}
	var sb strings.Builder
	for i := 0; i < len(token); i++ {
		ch := token[i]
		sb.WriteByte(ch)
		if ch == '.' && (i+1 >= len(token) || !isDigit(token[i+1])) {
			sb.WriteByte('0')
		}
	}
	mantissa := sb.String()
	if strings.HasPrefix(mantissa, ".") {
		mantissa = "0" + mantissa
	}
	c.out = append(c.out, sign...)
	c.out = append(c.out, mantissa...)
	return nil
}

// peekStructural returns the next significant byte after the current
// position, skipping whitespace and comments without consuming anything.
// It returns 0 at end of input (or an unterminated comment, which the main
// loop reports).
func (c *json5Converter) peekStructural() byte {
	i := c.pos
	for i < len(c.src) {
		switch b := c.src[i]; b {
		case ' ', '\t', '\n', '\r':
			i++
		case '/':
			if i+1 >= len(c.src) {
				return '/'
			}
			switch c.src[i+1] {
			case '/':
				i += 2
				for i < len(c.src) && c.src[i] != '\n' {
					i++
				}
			case '*':
				i += 2
				for i+1 < len(c.src) && !(c.src[i] == '*' && c.src[i+1] == '/') {
					i++
				}
				if i+1 >= len(c.src) {
					return 0
				}
				i += 2
			default:
				return '/'
			}
		default:
			return b
		}
	}
	return 0
}

// errorf reports a converter error with the 1-based line and column of pos.
func (c *json5Converter) errorf(pos int, format string, args ...any) error {
	line, col := lineColumn(c.src, pos)
	return fmt.Errorf("line %d, column %d: %s", line, col, fmt.Sprintf(format, args...))
}

// lineColumn returns the 1-based line and column of a byte offset in src.
func lineColumn(src []byte, pos int) (int, int) {
	line, col := 1, 1
	for i := 0; i < pos && i < len(src); i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// locateJSON5Error prefixes a decoding error with the original file's line
// and column when the error carries a byte offset: jsontext and json/v2
// errors report offsets in the converted text, which the offset table maps
// back to the source. Errors without a usable offset pass through unchanged;
// semantic errors keep their JSON pointer, which Error() already carries.
func locateJSON5Error(doc *json5Document, err error) string {
	var offset int64 = -1
	var syn *jsontext.SyntacticError
	var sem *json.SemanticError
	switch {
	case errors.As(err, &syn):
		offset = syn.ByteOffset
	case errors.As(err, &sem):
		offset = sem.ByteOffset
	}
	if offset < 0 || offset > int64(len(doc.json)) {
		return err.Error()
	}
	src, ok := doc.sourceOffset(int(offset))
	if !ok {
		return err.Error()
	}
	line, col := doc.lineColumn(src)
	return fmt.Sprintf("line %d, column %d: %s", line, col, err.Error())
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isHexDigit(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '$'
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || isDigit(b)
}

// isNumberPart accepts the characters a JSON5 number may span: digits, hex
// letters and the 0x prefix, the exponent marker, signs, and the decimal
// point.
func isNumberPart(b byte) bool {
	return isDigit(b) || isHexDigit(b) || b == 'x' || b == 'X' || b == '.' || b == '+' || b == '-'
}
