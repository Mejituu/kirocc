package customprov

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
)

// rewriteModel replaces the value of the top-level "model" member in a JSON
// request body, copying every other token verbatim. The body is rewritten
// token-by-token rather than decoded into a struct and re-marshalled so that
// key order, unknown fields, and number spellings Claude Code may rely on
// survive the pass unchanged.
func rewriteModel(raw []byte, model string) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)

	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	if tok.Kind() != '{' {
		return nil, fmt.Errorf("request body is not a JSON object")
	}
	if err := enc.WriteToken(tok); err != nil {
		return nil, err
	}

	// depth tracks container nesting relative to the root object; valueNext
	// tracks object parity at depth 1 so a "model" String token is only
	// treated as a key, never as a string value.
	depth := 1
	valueNext := false
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		kind := tok.Kind()

		if depth == 1 && kind == '"' && !valueNext {
			if tok.String() == "model" {
				// Write the key before reading the value: jsontext voids a
				// token once the decoder advances past it.
				if err := enc.WriteToken(tok); err != nil {
					return nil, err
				}
				value, err := dec.ReadToken()
				if err != nil {
					return nil, err
				}
				if value.Kind() != '"' {
					return nil, fmt.Errorf("request model is not a string")
				}
				if err := enc.WriteToken(jsontext.String(model)); err != nil {
					return nil, err
				}
				continue
			}
			valueNext = true
		} else if depth == 1 {
			// A value at depth 1 (including a container opening) is followed
			// by the next key; a container's closing token is read at
			// depth 2, so valueNext correctly stays false through nesting.
			valueNext = false
		}

		if err := enc.WriteToken(tok); err != nil {
			return nil, err
		}
		switch kind {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return buf.Bytes(), nil
			}
		}
	}
	return nil, fmt.Errorf("request body is truncated")
}
