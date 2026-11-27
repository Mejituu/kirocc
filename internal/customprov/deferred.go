package customprov

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"log/slog"

	"github.com/d-kuro/kirocc/internal/anthropic"
)

// errDeferredBody marks a messages member the streaming scan cannot classify;
// the scan then reports failure and no rewrite happens.
var errDeferredBody = errors.New("deferred: unexpected token in messages")

// deferredPolicy enables the Claude Code deferred-tool flattening pass.
type deferredPolicy struct {
	enabled bool
}

// deferredToolRef is one tool definition collected from a deferred-tool block,
// in first-appearance order and deduplicated by its original name.
type deferredToolRef struct {
	Name       string
	Definition jsontext.Value
}

// deferredPlan is the read-only pre-scan result: definitions to append to the
// top-level tools member, and a messages array already stripped of deferred
// blocks and moved out of tool_use/tool_result gaps.
type deferredPlan struct {
	defs            []deferredToolRef
	messages        jsontext.Value
	messagesChanged bool
}

func (p deferredPlan) empty() bool {
	return len(p.defs) == 0 && !p.messagesChanged
}

// deferredRefSet collects definitions lazily so a request without any
// tool_definition block does not allocate a name set.
type deferredRefSet struct {
	seen map[string]struct{}
	refs []deferredToolRef
}

// addDefinition records def when its name is non-empty and unseen. It returns
// the parsed name; ok is false only when the definition cannot be named.
func (s *deferredRefSet) addDefinition(def jsontext.Value) (name string, ok bool) {
	name = definitionName(def)
	if name == "" {
		slog.Warn("deferred tool definition has no name; dropping it")
		return "", false
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if _, exists := s.seen[name]; exists {
		return name, true
	}
	s.seen[name] = struct{}{}
	s.refs = append(s.refs, deferredToolRef{Name: name, Definition: def})
	return name, true
}

// deferredRole classifies a message's role during the streaming scan.
type deferredRole uint8

const (
	deferredRoleOther deferredRole = iota
	deferredRoleUser
	deferredRoleAssistant
	deferredRoleSystem
)

// deferredMessageSpan records one message's role and its exact byte range in
// the request body, so the fast path can reorder messages without decoding or
// copying them.
type deferredMessageSpan struct {
	role  deferredRole
	start int
	end   int
}

// deferredBody is the result of one streaming pass over the root object.
// Message contents are skipped, never copied.
type deferredBody struct {
	messages      []deferredMessageSpan
	messagesStart int
	messagesEnd   int
	messagesArray bool
	toolsStart    int
	toolsEnd      int
	hasTools      bool
	hasSystem     bool
	// unscannable is set when a messages element is not an object or a role
	// value is not a string. The old role probe rejected such bodies, so the
	// no-block fast path leaves them untouched and only the full path runs.
	unscannable bool
}

// analyzeDeferred pre-scans raw for the deferred-tool block names. Requests
// without any block only need rule 7's system-message moves: those are decided
// from the role sequence, and the moved messages are rebuilt from their
// original byte ranges. A request that carries a block name goes through the
// full rewrite in analyzeDeferredBlocks.
func analyzeDeferred(raw []byte) deferredPlan {
	hasBlock := bytes.Contains(raw, []byte(anthropic.BlockTypeToolAddition)) ||
		bytes.Contains(raw, []byte(anthropic.BlockTypeToolRemoval)) ||
		bytes.Contains(raw, []byte(anthropic.BlockTypeToolDefinition))
	body, ok := scanDeferredBody(raw, hasBlock)
	if !hasBlock {
		if !ok || !body.hasSystem || body.unscannable {
			return deferredPlan{}
		}
		moves := body.planMoves(raw)
		if len(moves) == 0 {
			return deferredPlan{}
		}
		return deferredPlan{messages: body.applyMoves(raw, moves), messagesChanged: true}
	}
	if !ok {
		return deferredPlan{}
	}
	return analyzeDeferredBlocks(raw, body)
}

// analyzeDeferredBlocks is the full deferred-tool pass: it decodes the messages
// array, rewrites every block, and applies rule 7 to the rewritten sequence. It
// is the production path for requests that carry a block and the reference
// oracle for the fast move-only path.
func analyzeDeferredBlocks(raw []byte, body deferredBody) deferredPlan {
	if !body.messagesArray {
		return deferredPlan{}
	}
	messages := jsontext.Value(raw[body.messagesStart:body.messagesEnd])
	var tools jsontext.Value
	if body.hasTools {
		tools = jsontext.Value(raw[body.toolsStart:body.toolsEnd])
	}
	refs, out, changed, err := scanDeferredMessages(messages)
	if err != nil {
		return deferredPlan{}
	}
	if len(refs) > 0 {
		refs = filterExistingTools(refs, tools)
	}
	if len(refs) == 0 && !changed {
		return deferredPlan{}
	}
	plan := deferredPlan{defs: refs, messagesChanged: changed}
	if changed {
		plan.messages = out
	}
	return plan
}

// scanDeferredBody streams the root object once: it records the byte ranges of
// the messages and tools members and each message's role, skipping every
// content value. Tools are only read when the full path may need them; the
// move-only path never decodes them. ok is false when the body is not a JSON
// object or does not parse.
func scanDeferredBody(raw []byte, needTools bool) (deferredBody, bool) {
	var body deferredBody
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.ReadToken()
	if err != nil || tok.Kind() != '{' {
		return body, false
	}
	for {
		tok, err = dec.ReadToken()
		if err != nil {
			return body, false
		}
		if tok.Kind() == '}' {
			return body, true
		}
		if tok.Kind() != '"' {
			return body, false
		}
		switch key := tok.String(); key {
		case "messages":
			if err := body.scanMessagesMember(dec, raw); err != nil {
				return body, false
			}
		case "tools":
			if !needTools {
				if err := dec.SkipValue(); err != nil {
					return body, false
				}
				continue
			}
			value, err := dec.ReadValue()
			if err != nil {
				return body, false
			}
			body.toolsStart = int(dec.InputOffset()) - len(value)
			body.toolsEnd = int(dec.InputOffset())
			body.hasTools = true
		default:
			if err := dec.SkipValue(); err != nil {
				return body, false
			}
		}
	}
}

// scanMessagesMember records the messages member's range and, for an array,
// every element's role and range. A non-array messages value is recorded whole
// for the full path.
func (b *deferredBody) scanMessagesMember(dec *jsontext.Decoder, raw []byte) error {
	b.messages = b.messages[:0]
	b.hasSystem = false
	b.messagesArray = false
	if dec.PeekKind() != '[' {
		value, err := dec.ReadValue()
		if err != nil {
			return err
		}
		b.messagesStart = int(dec.InputOffset()) - len(value)
		b.messagesEnd = int(dec.InputOffset())
		return nil
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	b.messagesStart = int(dec.InputOffset()) - 1
	b.messagesArray = true
	for {
		if dec.PeekKind() == ']' {
			if _, err := dec.ReadToken(); err != nil {
				return err
			}
			b.messagesEnd = int(dec.InputOffset())
			return nil
		}
		span, err := b.scanMessageSpan(dec, raw)
		if err != nil {
			return err
		}
		b.messages = append(b.messages, span)
		if span.role == deferredRoleSystem {
			b.hasSystem = true
		}
	}
}

// scanMessageSpan records one message's byte range and role. Non-object
// elements are recorded whole; their role stays other.
func (b *deferredBody) scanMessageSpan(dec *jsontext.Decoder, raw []byte) (deferredMessageSpan, error) {
	if dec.PeekKind() != '{' {
		value, err := dec.ReadValue()
		if err != nil {
			return deferredMessageSpan{}, err
		}
		b.unscannable = true
		end := int(dec.InputOffset())
		return deferredMessageSpan{start: end - len(value), end: end}, nil
	}
	if _, err := dec.ReadToken(); err != nil {
		return deferredMessageSpan{}, err
	}
	span := deferredMessageSpan{start: int(dec.InputOffset()) - 1}
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return deferredMessageSpan{}, err
		}
		if tok.Kind() == '}' {
			span.end = int(dec.InputOffset())
			return span, nil
		}
		if tok.Kind() != '"' {
			return deferredMessageSpan{}, errDeferredBody
		}
		if tok.String() == "role" && dec.PeekKind() == '"' {
			value, err := dec.ReadToken()
			if err != nil {
				return deferredMessageSpan{}, err
			}
			switch value.String() {
			case "user":
				span.role = deferredRoleUser
			case "assistant":
				span.role = deferredRoleAssistant
			case "system":
				span.role = deferredRoleSystem
			}
			continue
		}
		if tok.String() == "role" {
			b.unscannable = true
		}
		if err := dec.SkipValue(); err != nil {
			return deferredMessageSpan{}, err
		}
	}
}

// deferredMove says: move b.messages[run...] to immediately after
// b.messages[after].
type deferredMove struct {
	run   []int
	after int
}

// planMoves applies rule 7 from the role sequence alone: a system run whose
// neighbours are an assistant and a user is a candidate, and only those two
// boundary messages are decoded to check that the user answers one of the
// assistant's tool_use ids. Other shapes never decode a message.
func (b deferredBody) planMoves(raw []byte) []deferredMove {
	var moves []deferredMove
	for i := 0; i < len(b.messages); {
		if b.messages[i].role != deferredRoleSystem {
			i++
			continue
		}
		runEnd := i
		for runEnd < len(b.messages) && b.messages[runEnd].role == deferredRoleSystem {
			runEnd++
		}
		if i > 0 && runEnd < len(b.messages) &&
			b.messages[i-1].role == deferredRoleAssistant &&
			b.messages[runEnd].role == deferredRoleUser {
			assistant := jsontext.Value(raw[b.messages[i-1].start:b.messages[i-1].end])
			user := jsontext.Value(raw[b.messages[runEnd].start:b.messages[runEnd].end])
			if pending := messageToolUseIDs(assistant); len(pending) > 0 && messageAnswersToolUse(user, pending) {
				run := make([]int, 0, runEnd-i)
				for k := i; k < runEnd; k++ {
					run = append(run, k)
				}
				moves = append(moves, deferredMove{run: run, after: runEnd})
			}
		}
		i = runEnd
	}
	return moves
}

// applyMoves rebuilds the messages array in rule 7 order from the original byte
// ranges. The result matches joinArray over the decoded messages.
func (b deferredBody) applyMoves(raw []byte, moves []deferredMove) jsontext.Value {
	held := make(map[int]struct{})
	after := make(map[int][]int)
	for _, move := range moves {
		for _, index := range move.run {
			held[index] = struct{}{}
			after[move.after] = append(after[move.after], index)
		}
	}
	var buf bytes.Buffer
	buf.Grow(b.messagesEnd - b.messagesStart)
	buf.WriteByte('[')
	first := true
	write := func(index int) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(raw[b.messages[index].start:b.messages[index].end])
	}
	for i := range b.messages {
		if _, ok := held[i]; ok {
			continue
		}
		write(i)
		for _, index := range after[i] {
			write(index)
		}
	}
	buf.WriteByte(']')
	return jsontext.Value(buf.Bytes())
}

// scanDeferredMessages rewrites a messages array in one pass: it deletes
// tool_addition/tool_removal blocks, replaces tool_result tool_definition
// blocks with tool_reference, drops messages whose content array becomes
// empty, and then moves system messages out of tool_use/tool_result gaps.
// A messages array with nothing to change is returned as the original value.
func scanDeferredMessages(messages jsontext.Value) (refs []deferredToolRef, out jsontext.Value, changed bool, err error) {
	if messages.Kind() != jsontext.KindBeginArray {
		return nil, messages, false, nil
	}
	var msgs []jsontext.Value
	if err := json.Unmarshal(messages, &msgs); err != nil {
		return nil, nil, false, err
	}

	var set deferredRefSet
	var result []jsontext.Value
	for i, msg := range msgs {
		next, msgChanged, dropped, err := rewriteDeferredMessage(msg, &set)
		if err != nil {
			return nil, nil, false, err
		}
		if !msgChanged {
			if result != nil {
				result = append(result, msg)
			}
			continue
		}
		changed = true
		if result == nil {
			result = make([]jsontext.Value, 0, len(msgs))
			result = append(result, msgs[:i]...)
		}
		if !dropped {
			result = append(result, next)
		}
	}

	base := msgs
	if result != nil {
		base = result
	}
	if moves := planGapMoves(base); len(moves) > 0 {
		moved, _ := applyGapMoves(base, moves)
		base = moved
		changed = true
	}
	if !changed {
		return set.refs, messages, false, nil
	}
	return set.refs, joinArray(base), true, nil
}

// rewriteDeferredMessage rewrites one message's content array. It reports
// whether the message changed and whether it became empty (and must be
// dropped). dropped is false for a message that was already empty.
func rewriteDeferredMessage(msg jsontext.Value, set *deferredRefSet) (next jsontext.Value, changed, dropped bool, err error) {
	var message struct {
		Content jsontext.Value `json:"content"`
	}
	if err := json.Unmarshal(msg, &message); err != nil {
		return msg, false, false, nil
	}
	if len(message.Content) == 0 || message.Content.Kind() != jsontext.KindBeginArray {
		return msg, false, false, nil
	}
	var blocks []jsontext.Value
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return msg, false, false, nil
	}

	var out []jsontext.Value
	msgChanged := false
	for i, block := range blocks {
		rewritten, blockChanged, drop, err := rewriteDeferredBlock(block, set)
		if err != nil {
			return msg, false, false, err
		}
		if !blockChanged {
			if out != nil {
				out = append(out, block)
			}
			continue
		}
		msgChanged = true
		if out == nil {
			out = make([]jsontext.Value, 0, len(blocks))
			out = append(out, blocks[:i]...)
		}
		if !drop {
			out = append(out, rewritten)
		}
	}
	if !msgChanged {
		return msg, false, false, nil
	}
	if len(out) == 0 {
		return nil, true, true, nil
	}
	rewritten, err := replaceMember(msg, "content", joinArray(out))
	if err != nil {
		return msg, false, false, err
	}
	return rewritten, true, false, nil
}

// rewriteDeferredBlock applies rules 1-3 to one message-level block. drop
// means the block disappears; changed is true for both a drop and an in-place
// replacement.
func rewriteDeferredBlock(block jsontext.Value, set *deferredRefSet) (next jsontext.Value, changed, drop bool, err error) {
	var head struct {
		Type    string         `json:"type"`
		Tool    jsontext.Value `json:"tool"`
		Content jsontext.Value `json:"content"`
	}
	if err := json.Unmarshal(block, &head); err != nil {
		return block, false, false, nil
	}
	switch head.Type {
	case anthropic.BlockTypeToolAddition:
		var tool struct {
			Type       string         `json:"type"`
			Definition jsontext.Value `json:"definition"`
		}
		if len(head.Tool) > 0 {
			_ = json.Unmarshal(head.Tool, &tool)
		}
		if tool.Type == anthropic.BlockTypeToolDefinition &&
			len(tool.Definition) > 0 && tool.Definition.Kind() == jsontext.KindBeginObject {
			set.addDefinition(tool.Definition)
		}
		return nil, true, true, nil

	case anthropic.BlockTypeToolRemoval:
		return nil, true, true, nil

	case anthropic.BlockTypeToolResult:
		if len(head.Content) == 0 || head.Content.Kind() != jsontext.KindBeginArray {
			return block, false, false, nil
		}
		var nested []jsontext.Value
		if err := json.Unmarshal(head.Content, &nested); err != nil {
			return block, false, false, nil
		}

		var out []jsontext.Value
		nestedChanged := false
		for i, item := range nested {
			var inner struct {
				Type         string         `json:"type"`
				Definition   jsontext.Value `json:"definition"`
				CacheControl jsontext.Value `json:"cache_control"`
			}
			if err := json.Unmarshal(item, &inner); err != nil {
				if out != nil {
					out = append(out, item)
				}
				continue
			}
			if inner.Type != anthropic.BlockTypeToolDefinition {
				if out != nil {
					out = append(out, item)
				}
				continue
			}
			name := ""
			if len(inner.Definition) > 0 && inner.Definition.Kind() == jsontext.KindBeginObject {
				name, _ = set.addDefinition(inner.Definition)
			}
			nestedChanged = true
			if out == nil {
				out = make([]jsontext.Value, 0, len(nested))
				out = append(out, nested[:i]...)
			}
			if name == "" {
				continue
			}
			out = append(out, toolReferenceBlock(name, inner.CacheControl))
		}
		if !nestedChanged {
			return block, false, false, nil
		}
		rewritten, err := replaceMember(block, "content", joinArray(out))
		if err != nil {
			return block, false, false, err
		}
		return rewritten, true, false, nil

	default:
		return block, false, false, nil
	}
}

// collectDeferredToolRefs collects tool definitions from message content
// values without rewriting anything. It is used by the OpenAI path, which
// drops the original blocks during its own conversion.
func collectDeferredToolRefs(contents []jsontext.Value) ([]deferredToolRef, error) {
	var set deferredRefSet
	for _, content := range contents {
		if len(content) == 0 || content.Kind() != jsontext.KindBeginArray {
			continue
		}
		if !bytes.Contains(content, []byte(anthropic.BlockTypeToolDefinition)) {
			continue
		}
		var blocks []jsontext.Value
		if err := json.Unmarshal(content, &blocks); err != nil {
			return nil, err
		}
		for _, block := range blocks {
			collectDeferredBlockRefs(block, &set)
		}
	}
	return set.refs, nil
}

func collectDeferredBlockRefs(block jsontext.Value, set *deferredRefSet) {
	var head struct {
		Type    string         `json:"type"`
		Tool    jsontext.Value `json:"tool"`
		Content jsontext.Value `json:"content"`
	}
	if err := json.Unmarshal(block, &head); err != nil {
		return
	}
	switch head.Type {
	case anthropic.BlockTypeToolAddition:
		var tool struct {
			Type       string         `json:"type"`
			Definition jsontext.Value `json:"definition"`
		}
		if len(head.Tool) == 0 {
			return
		}
		if err := json.Unmarshal(head.Tool, &tool); err != nil {
			return
		}
		if tool.Type != anthropic.BlockTypeToolDefinition || len(tool.Definition) == 0 ||
			tool.Definition.Kind() != jsontext.KindBeginObject {
			return
		}
		set.addDefinition(tool.Definition)

	case anthropic.BlockTypeToolResult:
		if len(head.Content) == 0 || head.Content.Kind() != jsontext.KindBeginArray {
			return
		}
		var nested []jsontext.Value
		if err := json.Unmarshal(head.Content, &nested); err != nil {
			return
		}
		for _, item := range nested {
			var inner struct {
				Type       string         `json:"type"`
				Definition jsontext.Value `json:"definition"`
			}
			if err := json.Unmarshal(item, &inner); err != nil {
				continue
			}
			if inner.Type == anthropic.BlockTypeToolDefinition && len(inner.Definition) > 0 &&
				inner.Definition.Kind() == jsontext.KindBeginObject {
				set.addDefinition(inner.Definition)
			}
		}
	}
}

// definitionName extracts the name field of a raw tool definition object.
func definitionName(definition jsontext.Value) string {
	var def struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return ""
	}
	return def.Name
}

// toolReferenceBlock builds the in-place replacement for a tool_definition
// block. cache_control, when present, moves to the replacement block so a
// prompt-cache breakpoint survives the rewrite.
func toolReferenceBlock(name string, cacheControl jsontext.Value) jsontext.Value {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"tool_reference","tool_name":`)
	nameJSON, _ := json.Marshal(name)
	buf.Write(nameJSON)
	if len(cacheControl) > 0 && cacheControl.Kind() != jsontext.KindNull {
		buf.WriteString(`,"cache_control":`)
		buf.Write(cacheControl)
	}
	buf.WriteByte('}')
	return jsontext.Value(buf.Bytes())
}

// filterExistingTools removes definitions whose name already exists in the
// top-level tools array; the existing definition wins (rule 1).
func filterExistingTools(refs []deferredToolRef, tools jsontext.Value) []deferredToolRef {
	if len(refs) == 0 || len(tools) == 0 || tools.Kind() != jsontext.KindBeginArray {
		return refs
	}
	var entries []jsontext.Value
	if err := json.Unmarshal(tools, &entries); err != nil {
		return refs
	}
	have := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var tool struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(entry, &tool); err == nil && tool.Name != "" {
			have[tool.Name] = struct{}{}
		}
	}
	if len(have) == 0 {
		return refs
	}
	out := make([]deferredToolRef, 0, len(refs))
	for _, ref := range refs {
		if _, exists := have[ref.Name]; !exists {
			out = append(out, ref)
		}
	}
	return out
}

// appendToolDefinitions appends the collected definitions to the tools member.
// A missing or null member becomes a fresh array; any other non-array shape is
// left untouched. The caller only reaches this when defs is non-empty.
func appendToolDefinitions(tools jsontext.Value, defs []deferredToolRef) (jsontext.Value, bool) {
	if len(defs) == 0 {
		return tools, false
	}
	if len(tools) > 0 && tools.Kind() != jsontext.KindNull && tools.Kind() != jsontext.KindBeginArray {
		return tools, false
	}
	var entries []jsontext.Value
	if len(tools) > 0 && tools.Kind() == jsontext.KindBeginArray {
		if err := json.Unmarshal(tools, &entries); err != nil {
			return tools, false
		}
	}
	merged := make([]jsontext.Value, 0, len(entries)+len(defs))
	merged = append(merged, entries...)
	for _, def := range defs {
		merged = append(merged, def.Definition)
	}
	return joinArray(merged), true
}

// gapMove says: move msgs[run...] to immediately after msgs[after].
type gapMove struct {
	run   []int
	after int
}

// planGapMoves finds system-message runs that sit between an assistant's
// tool_use blocks and a user message answering them (rule 7). Only the run
// immediately following the assistant is considered; other roles break it.
func planGapMoves(msgs []jsontext.Value) []gapMove {
	var moves []gapMove
	var pending map[string]struct{}
	var run []int
	for i, msg := range msgs {
		role := messageRole(msg)
		switch role {
		case "assistant":
			pending = messageToolUseIDs(msg)
			if len(pending) == 0 {
				pending = nil
			}
			run = nil
		case "system":
			if pending != nil {
				run = append(run, i)
			}
		case "user":
			if pending != nil && len(run) > 0 && messageAnswersToolUse(msg, pending) {
				runCopy := append([]int(nil), run...)
				moves = append(moves, gapMove{run: runCopy, after: i})
			}
			pending = nil
			run = nil
		default:
			pending = nil
			run = nil
		}
	}
	return moves
}

// applyGapMoves rebuilds the messages array with the planned moves applied.
func applyGapMoves(msgs []jsontext.Value, moves []gapMove) ([]jsontext.Value, bool) {
	if len(moves) == 0 {
		return msgs, false
	}
	held := make(map[int]struct{})
	after := make(map[int][]jsontext.Value)
	for _, move := range moves {
		for _, index := range move.run {
			held[index] = struct{}{}
			if index >= 0 && index < len(msgs) {
				after[move.after] = append(after[move.after], msgs[index])
			}
		}
	}
	out := make([]jsontext.Value, 0, len(msgs))
	for i, msg := range msgs {
		if _, exists := held[i]; exists {
			continue
		}
		out = append(out, msg)
		if moved, exists := after[i]; exists {
			out = append(out, moved...)
		}
	}
	return out, true
}

func messageRole(msg jsontext.Value) string {
	var message struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(msg, &message); err != nil {
		return ""
	}
	return message.Role
}

// messageToolUseIDs returns the ids of the tool_use blocks in an assistant
// message; nil when the message has none.
func messageToolUseIDs(msg jsontext.Value) map[string]struct{} {
	var message struct {
		Content jsontext.Value `json:"content"`
	}
	if err := json.Unmarshal(msg, &message); err != nil ||
		len(message.Content) == 0 || message.Content.Kind() != jsontext.KindBeginArray {
		return nil
	}
	var blocks []jsontext.Value
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil
	}
	ids := make(map[string]struct{})
	for _, block := range blocks {
		var head struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(block, &head); err != nil {
			continue
		}
		if head.Type == anthropic.BlockTypeToolUse && head.ID != "" {
			ids[head.ID] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// messageAnswersToolUse reports whether a user message carries a tool_result
// whose id is in pending.
func messageAnswersToolUse(msg jsontext.Value, pending map[string]struct{}) bool {
	var message struct {
		Content jsontext.Value `json:"content"`
	}
	if err := json.Unmarshal(msg, &message); err != nil ||
		len(message.Content) == 0 || message.Content.Kind() != jsontext.KindBeginArray {
		return false
	}
	var blocks []jsontext.Value
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return false
	}
	for _, block := range blocks {
		var head struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(block, &head); err != nil {
			continue
		}
		if head.Type != anthropic.BlockTypeToolResult {
			continue
		}
		if _, exists := pending[head.ToolUseID]; exists {
			return true
		}
	}
	return false
}
