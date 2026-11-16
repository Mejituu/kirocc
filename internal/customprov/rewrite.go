package customprov

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/d-kuro/kirocc/internal/reqconv"
)

// RewriteModel replaces the value of the top-level "model" member in a JSON
// request body, copying every other token verbatim. The body is rewritten
// token-by-token rather than decoded into a struct and re-marshalled so that
// key order, unknown fields, and number spellings Claude Code may rely on
// survive the pass unchanged. Exported for the session-following mappings,
// which rewrite a request bound for the Kiro path.
func RewriteModel(raw []byte, model string) ([]byte, error) {
	return rewrite(raw, model, false)
}

// RewriteForward is the rewrite the anthropic-protocol Forward applies:
// it replaces the model like RewriteModel and additionally drops Claude
// Code's billing-header blocks from a top-level system array. The block's
// per-request cch value would break the upstream's prompt-prefix cache, so
// forwarding it costs cache hits without carrying instructions; the Kiro
// path strips the same blocks (see internal/reqconv/content_text.go).
// A system string is kept as-is, and a system array left empty by the
// removals drops the member entirely. Everything else is copied verbatim.
func RewriteForward(raw []byte, model string) ([]byte, error) {
	return rewrite(raw, model, true)
}

// rewrite carries out the token-level pass. stripBilling enables the
// top-level system cleanup; model is "" when the model member is not
// rewritten.
func rewrite(raw []byte, model string, stripBilling bool) ([]byte, error) {
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
	// tracks object parity at depth 1 so a "model" or "system" String token
	// is only treated as a key, never as a string value.
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
			key := tok.String()
			if key == "model" && model != "" {
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
			if key == "system" && stripBilling {
				// rewriteSystem consumed the whole value (ReadValue), so the
				// next token is again a key: valueNext stays false, like the
				// model branch's ReadToken-then-continue.
				if err := rewriteSystem(dec, enc); err != nil {
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

// rewriteSystem handles the value of a top-level "system" member when
// billing blocks are stripped. Neither the key nor the value has been
// written to enc yet: the value is decoded into a scratch buffer so the
// member can be dropped entirely when an array empties out; otherwise key
// and (rewritten) value are appended. A non-array value is copied through
// unchanged.
func rewriteSystem(dec *jsontext.Decoder, enc *jsontext.Encoder) error {
	value, err := dec.ReadValue()
	if err != nil {
		return err
	}
	if value.Kind() != jsontext.KindBeginArray {
		// String (or object/null): not the array form, keep as-is.
		if err := enc.WriteToken(jsontext.String("system")); err != nil {
			return err
		}
		return enc.WriteValue(value)
	}

	// Array form: filter the billing-header blocks, keep everything else
	// verbatim, and drop the whole member when nothing remains. Elements are
	// re-joined by hand rather than through a nested Encoder: WriteValue on a
	// member value preserves the exact bytes of what it is handed (number
	// spellings, key order, string escapes), so the hand-built array is
	// copied through equally faithfully. Elements keep their original bytes
	// because they are copied into scratch before the decoder advances again
	// (a ReadValue result is only valid until the next read).
	var elems []jsontext.Value
	if err := json.Unmarshal(value, &elems); err != nil {
		return err
	}
	var scratch bytes.Buffer
	scratch.WriteByte('[')
	n := 0
	for _, elem := range elems {
		if isBillingElem(elem) {
			continue
		}
		if n > 0 {
			scratch.WriteByte(',')
		}
		scratch.Write(elem)
		n++
	}
	scratch.WriteByte(']')
	if n == 0 {
		return nil // drop the member entirely
	}
	if err := enc.WriteToken(jsontext.String("system")); err != nil {
		return err
	}
	return enc.WriteValue(jsontext.Value(scratch.Bytes()))
}

// isBillingElem reports whether one system-array element is Claude Code's
// billing-header block: a text block whose text is the header alone.
func isBillingElem(elem jsontext.Value) bool {
	var block struct {
		Type string `json:"type,omitzero"`
		Text string `json:"text,omitzero"`
	}
	if err := json.Unmarshal(elem, &block); err != nil {
		return false // not a known block shape: keep it
	}
	return block.Type == "text" && reqconv.IsBillingHeaderBlock(block.Text)
}
