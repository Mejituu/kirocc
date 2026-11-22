package customprov

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/reqconv"
)

// mediaPolicy selects which content blocks the rewrite replaces.
type mediaPolicy struct {
	replaceImages    bool
	replaceDocuments bool
}

// enabled reports whether the policy has any work to do.
func (p mediaPolicy) enabled() bool {
	return p.replaceImages || p.replaceDocuments
}

// mediaCounts reports how many blocks a rewrite replaced.
type mediaCounts struct {
	images    int
	documents int
}

// add accumulates another pass's counts.
func (c *mediaCounts) add(other mediaCounts) {
	c.images += other.images
	c.documents += other.documents
}

// empty reports whether nothing was replaced.
func (c mediaCounts) empty() bool {
	return c.images == 0 && c.documents == 0
}

const (
	imageOmittedMarker    = "[image omitted]"
	documentOmittedMarker = "[document omitted]"
	blockTypeDocument     = "document"
)

// RewriteModel replaces the value of the top-level "model" member in a JSON
// request body, copying every other token verbatim. The body is rewritten
// token-by-token rather than decoded into a struct and re-marshalled so that
// key order, unknown fields, and number spellings Claude Code may rely on
// survive the pass unchanged. Exported for the session-following mappings,
// which rewrite a request bound for the Kiro path.
func RewriteModel(raw []byte, model string) ([]byte, error) {
	out, _, err := rewrite(raw, model, false, false, mediaPolicy{})
	return out, err
}

// webSearchBillingText is the fixed line Bailian's native web_search gates
// on: the tool only activates when the system prompt carries this exact
// text, whose shape mimics Claude Code's billing header.
const webSearchBillingText = "x-anthropic-billing-header: cc_entrypoint=cli;"

// webSearchBillingBlock is the system-array element carrying
// webSearchBillingText. The text satisfies reqconv.IsBillingHeaderBlock
// itself, so the block may only join the system after the strip pass has
// removed the per-request blocks, or it would be dropped with them.
const webSearchBillingBlock = `{"type":"text","text":"` + webSearchBillingText + `"}`

// webSearchSystemAlone is the system value a body without a system member
// gets: the fixed block on its own.
const webSearchSystemAlone = `[` + webSearchBillingBlock + `]`

// RewriteForward is the rewrite the anthropic-protocol Forward applies:
// it replaces the model like RewriteModel and additionally drops Claude
// Code's billing-header blocks from a top-level system array. The block's
// per-request cch value would break the upstream's prompt-prefix cache, so
// forwarding it costs cache hits without carrying instructions; the Kiro
// path strips the same blocks (see internal/reqconv/content_text.go).
// A system string is kept as-is, and a system array left empty by the
// removals drops the member entirely. Everything else is copied verbatim.
//
// webSearch additionally plants webSearchBillingBlock in the system: the
// strip pass runs first, then the block becomes the first element (a
// string system is promoted to the array form around it, a body without a
// system member gets one appended). With the block present the system
// member is never dropped, even when stripping empties the original array.
//
// media replaces image/document blocks in messages and returns their counts.
// A body with nothing to replace is written exactly as the media-disabled
// pass writes it.
func RewriteForward(raw []byte, model string, webSearch bool, media mediaPolicy) ([]byte, mediaCounts, error) {
	return rewrite(raw, model, true, webSearch, media)
}

// rewrite carries out the token-level pass. stripBilling enables the
// top-level system cleanup; insertHeader plants the web-search billing
// block in the system (only RewriteForward sets it, always alongside
// stripBilling, since the block must follow the strip pass); model is ""
// when the model member is not rewritten. media enables the messages pass.
func rewrite(raw []byte, model string, stripBilling, insertHeader bool, media mediaPolicy) ([]byte, mediaCounts, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)

	tok, err := dec.ReadToken()
	if err != nil {
		return nil, mediaCounts{}, err
	}
	if tok.Kind() != '{' {
		return nil, mediaCounts{}, fmt.Errorf("request body is not a JSON object")
	}
	if err := enc.WriteToken(tok); err != nil {
		return nil, mediaCounts{}, err
	}

	// depth tracks container nesting relative to the root object; valueNext
	// tracks object parity at depth 1 so a "model" or "system" String token
	// is only treated as a key, never as a string value; sawSystem records
	// whether a top-level system member went by, so the closing-brace
	// insertion below only fires for bodies that never had one.
	depth := 1
	valueNext := false
	sawSystem := false
	var counts mediaCounts
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, mediaCounts{}, err
		}
		kind := tok.Kind()

		if depth == 1 && kind == '"' && !valueNext {
			key := tok.String()
			if key == "system" {
				sawSystem = true
			}
			if key == "model" && model != "" {
				// Write the key before reading the value: jsontext voids a
				// token once the decoder advances past it.
				if err := enc.WriteToken(tok); err != nil {
					return nil, mediaCounts{}, err
				}
				value, err := dec.ReadToken()
				if err != nil {
					return nil, mediaCounts{}, err
				}
				if value.Kind() != '"' {
					return nil, mediaCounts{}, fmt.Errorf("request model is not a string")
				}
				if err := enc.WriteToken(jsontext.String(model)); err != nil {
					return nil, mediaCounts{}, err
				}
				continue
			}
			if key == "system" && stripBilling {
				// rewriteSystem consumed the whole value (ReadValue), so the
				// next token is again a key: valueNext stays false, like the
				// model branch's ReadToken-then-continue.
				if err := rewriteSystem(dec, enc, insertHeader); err != nil {
					return nil, mediaCounts{}, err
				}
				continue
			}
			if key == "messages" && media.enabled() {
				value, err := dec.ReadValue()
				if err != nil {
					return nil, mediaCounts{}, err
				}
				processed, replaced, changed, err := rewriteMessages(value, media)
				if err != nil {
					return nil, mediaCounts{}, err
				}
				// Write the key after the read for the same reason as
				// rewriteSystem: tok is void once the decoder advances.
				if err := enc.WriteToken(jsontext.String("messages")); err != nil {
					return nil, mediaCounts{}, err
				}
				if changed {
					if err := enc.WriteValue(processed); err != nil {
						return nil, mediaCounts{}, err
					}
					counts.add(replaced)
				} else if err := replayValue(enc, value); err != nil {
					return nil, mediaCounts{}, err
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

		// The root object is closing without a system member: plant the
		// web-search block as the last member. The only '}' read at depth 1
		// is the root's, and the Encoder supplies the member separator, so
		// no comma bookkeeping is needed here.
		if insertHeader && !sawSystem && depth == 1 && kind == '}' {
			if err := enc.WriteToken(jsontext.String("system")); err != nil {
				return nil, mediaCounts{}, err
			}
			if err := enc.WriteValue(jsontext.Value(webSearchSystemAlone)); err != nil {
				return nil, mediaCounts{}, err
			}
		}

		if err := enc.WriteToken(tok); err != nil {
			return nil, mediaCounts{}, err
		}
		switch kind {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return buf.Bytes(), counts, nil
			}
		}
	}
	return nil, mediaCounts{}, fmt.Errorf("request body is truncated")
}

// rewriteSystem handles the value of a top-level "system" member when
// billing blocks are stripped. Neither the key nor the value has been
// written to enc yet: the value is decoded into a scratch buffer so the
// member can be dropped entirely when an array empties out; otherwise key
// and (rewritten) value are appended. insertHeader additionally plants the
// web-search billing block: a string system is promoted to the array form
// around it, and an array always keeps the member with the block first, so
// the emptied-out drop no longer applies. Other non-array shapes (object,
// null) are invalid for the upstream anyway and copied through unchanged,
// web search or not.
func rewriteSystem(dec *jsontext.Decoder, enc *jsontext.Encoder, insertHeader bool) error {
	value, err := dec.ReadValue()
	if err != nil {
		return err
	}
	if insertHeader && value.Kind() == jsontext.KindString {
		// Promote the string to the array form: the fixed block first, the
		// string as a text block second. The string's raw JSON bytes (quotes
		// and escape spellings included) are embedded in the text field
		// verbatim, exactly like the array elements below.
		var scratch bytes.Buffer
		scratch.WriteByte('[')
		scratch.WriteString(webSearchBillingBlock)
		scratch.WriteString(`,{"type":"text","text":`)
		scratch.Write(value)
		scratch.WriteString(`}]`)
		if err := enc.WriteToken(jsontext.String("system")); err != nil {
			return err
		}
		return enc.WriteValue(jsontext.Value(scratch.Bytes()))
	}
	if value.Kind() != jsontext.KindBeginArray {
		// String (without insertion), object, or null: not the array form,
		// keep as-is.
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
	// (a ReadValue result is only valid until the next read). With
	// insertHeader the fixed block is written first and the filter below
	// only ever sees the original elements, so the strip pass runs to
	// completion before the block joins — including when an original element
	// happens to look exactly like it.
	var elems []jsontext.Value
	if err := json.Unmarshal(value, &elems); err != nil {
		return err
	}
	var scratch bytes.Buffer
	scratch.WriteByte('[')
	n := 0
	if insertHeader {
		scratch.WriteString(webSearchBillingBlock)
		n++
	}
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

// replayValue writes value's tokens through enc, reproducing the generic
// token pass for a member the media pass left untouched.
func replayValue(enc *jsontext.Encoder, value jsontext.Value) error {
	dec := jsontext.NewDecoder(bytes.NewReader(value))
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := enc.WriteToken(tok); err != nil {
			return err
		}
	}
}

// rewriteMessages replaces media blocks in the top-level messages array. It
// returns the original value unchanged when no block needs replacing, so the
// caller can replay it through the normal token pass and keep byte output
// identical to a media-disabled rewrite.
func rewriteMessages(value jsontext.Value, media mediaPolicy) (jsontext.Value, mediaCounts, bool, error) {
	if value.Kind() != jsontext.KindBeginArray {
		return value, mediaCounts{}, false, nil
	}
	var messages []jsontext.Value
	if err := json.Unmarshal(value, &messages); err != nil {
		return nil, mediaCounts{}, false, err
	}
	var out []jsontext.Value
	var counts mediaCounts
	changed := false
	for i, msg := range messages {
		next, replaced, msgChanged, err := rewriteMessage(msg, media)
		if err != nil {
			return nil, mediaCounts{}, false, err
		}
		if !msgChanged {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		changed = true
		counts.add(replaced)
		if out == nil {
			out = make([]jsontext.Value, i, len(messages))
			copy(out, messages[:i])
		}
		out = append(out, next)
	}
	if !changed {
		return value, mediaCounts{}, false, nil
	}
	return joinArray(out), counts, true, nil
}

// rewriteMessage rewrites one message object's content array. A message with
// no replaceable media is returned byte-for-byte unchanged.
func rewriteMessage(msg jsontext.Value, media mediaPolicy) (jsontext.Value, mediaCounts, bool, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(msg))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, mediaCounts{}, false, err
	}
	if tok.Kind() != '{' {
		return msg, mediaCounts{}, false, nil
	}
	if err := enc.WriteToken(tok); err != nil {
		return nil, mediaCounts{}, false, err
	}
	depth := 1
	valueNext := false
	var counts mediaCounts
	changed := false
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, mediaCounts{}, false, err
		}
		kind := tok.Kind()
		if depth == 1 && kind == '"' && !valueNext {
			if tok.String() == "content" {
				value, err := dec.ReadValue()
				if err != nil {
					return nil, mediaCounts{}, false, err
				}
				if err := enc.WriteToken(jsontext.String("content")); err != nil {
					return nil, mediaCounts{}, false, err
				}
				if value.Kind() == jsontext.KindBeginArray {
					next, replaced, contentChanged, err := rewriteContentArray(value, media, true)
					if err != nil {
						return nil, mediaCounts{}, false, err
					}
					if contentChanged {
						changed = true
						counts.add(replaced)
						if err := enc.WriteValue(next); err != nil {
							return nil, mediaCounts{}, false, err
						}
					} else if err := replayValue(enc, value); err != nil {
						return nil, mediaCounts{}, false, err
					}
				} else if err := enc.WriteValue(value); err != nil {
					return nil, mediaCounts{}, false, err
				}
				continue
			}
			valueNext = true
		} else if depth == 1 {
			valueNext = false
		}
		if err := enc.WriteToken(tok); err != nil {
			return nil, mediaCounts{}, false, err
		}
		switch kind {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if !changed {
		return msg, mediaCounts{}, false, nil
	}
	return trimEncoderNewline(&buf), counts, true, nil
}

// rewriteContentArray rewrites image/document blocks in a content array and
// recurses into tool_result blocks. A content array with no replaceable media
// is returned byte-for-byte unchanged.
func rewriteContentArray(value jsontext.Value, media mediaPolicy, allowToolResult bool) (jsontext.Value, mediaCounts, bool, error) {
	var blocks []jsontext.Value
	if err := json.Unmarshal(value, &blocks); err != nil {
		return nil, mediaCounts{}, false, err
	}
	var out []jsontext.Value
	var counts mediaCounts
	changed := false
	for i, block := range blocks {
		next, replaced, blockChanged, err := rewriteBlock(block, media, allowToolResult)
		if err != nil {
			return nil, mediaCounts{}, false, err
		}
		if !blockChanged {
			if out != nil {
				out = append(out, block)
			}
			continue
		}
		changed = true
		counts.add(replaced)
		if out == nil {
			out = make([]jsontext.Value, i, len(blocks))
			copy(out, blocks[:i])
		}
		out = append(out, next)
	}
	if !changed {
		return value, mediaCounts{}, false, nil
	}
	return joinArray(out), counts, true, nil
}

// rewriteBlock replaces an image or document block with a text marker, or
// rewrites the nested content of a tool_result block. cache_control moves to
// the marker block verbatim so a prompt-cache breakpoint on the media block
// survives the replacement.
func rewriteBlock(block jsontext.Value, media mediaPolicy, allowToolResult bool) (jsontext.Value, mediaCounts, bool, error) {
	var head struct {
		Type         string         `json:"type,omitzero"`
		CacheControl jsontext.Value `json:"cache_control,omitzero"`
		Content      jsontext.Value `json:"content,omitzero"`
	}
	if err := json.Unmarshal(block, &head); err != nil {
		return block, mediaCounts{}, false, nil
	}
	switch head.Type {
	case anthropic.BlockTypeImage:
		if !media.replaceImages {
			return block, mediaCounts{}, false, nil
		}
		return markerBlock(imageOmittedMarker, head.CacheControl), mediaCounts{images: 1}, true, nil
	case blockTypeDocument:
		if !media.replaceDocuments {
			return block, mediaCounts{}, false, nil
		}
		return markerBlock(documentOmittedMarker, head.CacheControl), mediaCounts{documents: 1}, true, nil
	case anthropic.BlockTypeToolResult:
		// Only the message-level pass descends into tool_result; a nested
		// tool_result is not a shape Anthropic produces and stays untouched.
		if !allowToolResult || len(head.Content) == 0 || head.Content.Kind() != jsontext.KindBeginArray {
			return block, mediaCounts{}, false, nil
		}
		next, counts, changed, err := rewriteContentArray(head.Content, media, false)
		if err != nil || !changed {
			return block, mediaCounts{}, false, err
		}
		replaced, err := replaceMember(block, "content", next)
		if err != nil {
			return nil, mediaCounts{}, false, err
		}
		return replaced, counts, true, nil
	default:
		return block, mediaCounts{}, false, nil
	}
}

// replaceMember rewrites one top-level member of an object, copying every
// other token verbatim.
func replaceMember(value jsontext.Value, name string, replacement jsontext.Value) (jsontext.Value, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(value))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	if tok.Kind() != '{' {
		return value, nil
	}
	if err := enc.WriteToken(tok); err != nil {
		return nil, err
	}
	depth := 1
	valueNext := false
	replaced := false
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
			if tok.String() == name && !replaced {
				if _, err := dec.ReadValue(); err != nil {
					return nil, err
				}
				if err := enc.WriteToken(jsontext.String(name)); err != nil {
					return nil, err
				}
				if err := enc.WriteValue(replacement); err != nil {
					return nil, err
				}
				replaced = true
				continue
			}
			valueNext = true
		} else if depth == 1 {
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
		}
		if depth == 0 {
			break
		}
	}
	return trimEncoderNewline(&buf), nil
}

// markerBlock builds the text block that replaces a media block.
func markerBlock(marker string, cacheControl jsontext.Value) jsontext.Value {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"` + anthropic.BlockTypeText + `","text":"`)
	buf.WriteString(marker)
	buf.WriteString(`"`)
	if len(cacheControl) > 0 && cacheControl.Kind() != jsontext.KindNull {
		buf.WriteString(`,"cache_control":`)
		buf.Write(cacheControl)
	}
	buf.WriteByte('}')
	return jsontext.Value(buf.Bytes())
}

// joinArray joins raw values into a compact JSON array.
func joinArray(values []jsontext.Value) jsontext.Value {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(value)
	}
	buf.WriteByte(']')
	return jsontext.Value(buf.Bytes())
}

// trimEncoderNewline removes the newline jsontext.Encoder appends after a
// top-level value; nested message/block encoders must not leak it into parent
// JSON.
func trimEncoderNewline(buf *bytes.Buffer) jsontext.Value {
	b := buf.Bytes()
	b = bytes.TrimSuffix(b, []byte("\n"))
	return jsontext.Value(b)
}
