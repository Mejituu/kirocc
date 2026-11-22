package customprov

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/timing"
)

// forwardOpenAI sends an Anthropic request converted to Chat Completions to
// the provider and writes the converted response to w. It is the openai
// counterpart of Forward; see Forward for the argument meanings. The client's
// headers are deliberately not forwarded: the provider speaks the OpenAI
// protocol, so the request carries only that protocol's own headers.
func (p *Proxy) forwardOpenAI(ctx context.Context, w http.ResponseWriter, h http.Header, raw []byte, hit Hit, info RequestInfo, endpoint Endpoint) {
	_, short := logging.TraceIDs(ctx)
	// Same timer arrangement as Forward: maybeForwardCustom put the timer in
	// ctx; forwardOpenAI is also called directly in tests, hence the fallback.
	timer := timing.From(ctx)
	if timer == nil {
		ctx, timer = timing.Start(ctx)
	}
	logPair := endpoint == EndpointMessages
	if logPair {
		slog.InfoContext(ctx, "--> POST /v1/messages", requestLogArgs(ctx, short, hit, info)...)
	}

	// Server-side tools (web_search, tool_search, advisor) execute in the
	// Anthropic backend, which an OpenAI-protocol provider cannot stand in
	// for. Fail the request up front instead of sending one that can only
	// fail downstream. Web_search is the one exception: a model that declared
	// it (hit) hands the tool to the provider's native search via
	// enable_search below. A decode error is left to buildOpenAIRequest, the
	// authoritative parser below.
	var toolPeek struct {
		Tools []anthropic.Tool `json:"tools,omitempty"`
	}
	if err := json.Unmarshal(raw, &toolPeek); err == nil {
		for i := range toolPeek.Tools {
			if !toolPeek.Tools[i].IsServerTool() {
				continue
			}
			if toolPeek.Tools[i].IsWebSearchTool() && hit.WebSearch {
				continue
			}
			httpx.WriteError(w, http.StatusBadRequest, httpx.ErrTypeInvalidRequest,
				"OpenAI-protocol custom providers do not support server-side tools; use an anthropic-protocol provider or disallow the tool (e.g. claude --disallowedTools WebSearch)")
			return
		}
	}

	oaiReq, counts, err := buildOpenAIRequest(raw, hit, info)
	if err != nil {
		slog.ErrorContext(ctx, "custom provider request conversion failed", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, httpx.ErrTypeInvalidRequest, "invalid request: "+err.Error())
		return
	}
	reason := ""
	if hit.TextOnly {
		reason = "declared"
	}
	if counts.images > 0 {
		logMediaReplaced(ctx, short, hit, counts, reason)
	}
	// One warning per request, whatever the count: the conversion replaced
	// document blocks (PDF and the like) the Chat protocol cannot carry. The
	// payload stays out — only the count is logged.
	if counts.documents > 0 {
		args := appendSessionID(ctx, []any{"trace_id", short})
		args = append(args, "provider", hit.Provider.Name, "count", counts.documents)
		slog.WarnContext(ctx, "custom provider document dropped", args...)
	}
	// The web_search exception to the toolPeek gate above, mirroring
	// Forward's A1 trigger: the model declared web_search (hit) and the
	// request carries the tool, so the provider's native search stands in
	// via Bailian's enable_search; forced_search keeps it on unconditionally.
	if info.HasWebSearch() && hit.WebSearch {
		oaiReq.EnableSearch = true
		oaiReq.SearchOptions = &oaiSearchOptions{ForcedSearch: true}
		slog.InfoContext(ctx, "web search enabled", "trace_id", short, "provider", hit.Provider.Name, "mode", "enable_search")
	}
	body, err := json.Marshal(oaiReq)
	if err != nil {
		slog.ErrorContext(ctx, "custom provider request conversion failed", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, httpx.ErrTypeInvalidRequest, "invalid request: "+err.Error())
		return
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.DebugContext(ctx, "custom provider request body", "trace_id", short, "request_body", jsontext.Value(body))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hit.Provider.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "custom provider request build failed", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadGateway, httpx.ErrTypeAPI, fmt.Sprintf("custom provider %q request could not be built", hit.Provider.Name))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if info.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	req.Header.Set("Authorization", "Bearer "+hit.Provider.authToken)
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.DebugContext(ctx, "custom provider request headers",
			"trace_id", short, "headers", logging.SafeHeaders{H: req.Header})
	}

	resp, err := p.client.Do(req)
	if err != nil {
		// A canceled context means the client went away; there is nobody left
		// to answer, so end quietly instead of writing to a dead connection.
		if ctx.Err() != nil {
			return
		}
		args := appendSessionID(ctx, []any{"trace_id", short})
		args = append(args, "provider", hit.Provider.Name, "err", err)
		slog.ErrorContext(ctx, "custom provider unreachable", args...)
		httpx.WriteError(w, http.StatusBadGateway, httpx.ErrTypeAPI, fmt.Sprintf("custom provider %q is unreachable", hit.Provider.Name))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The whole body is read so openAIErrorMessage sees it; the warning
		// logs only the first errBodyLimit bytes, like the anthropic path.
		fullBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			slog.ErrorContext(ctx, "custom provider response read failed",
				"trace_id", short, "provider", hit.Provider.Name, "err", readErr)
		} else if logPair {
			writeCompletionLog(ctx, short, hit, resp.StatusCode, nil, timer, false)
		}
		logged := fullBody
		if len(logged) > errBodyLimit {
			logged = logged[:errBodyLimit]
		}
		args := appendSessionID(ctx, []any{"trace_id", short})
		args = append(args, "provider", hit.Provider.Name, "status", resp.StatusCode, "body", string(logged))
		slog.WarnContext(ctx, "custom provider error", args...)
		// Retry-After is the one upstream error header worth passing on: a
		// rate-limited client backs off correctly instead of guessing. It
		// must be set before WriteError, which writes the header block.
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		httpx.WriteError(w, resp.StatusCode, oaiErrorType(resp.StatusCode), openAIErrorMessage(fullBody))
		return
	}

	if !info.Stream {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			slog.ErrorContext(ctx, "custom provider response read failed",
				"trace_id", short, "provider", hit.Provider.Name, "err", readErr)
			httpx.WriteError(w, http.StatusBadGateway, httpx.ErrTypeAPI, fmt.Sprintf("custom provider %q response could not be read", hit.Provider.Name))
			return
		}
		anthropicBody, u, convErr := openAIToAnthropicResponse(ctx, body)
		if convErr != nil {
			slog.ErrorContext(ctx, "custom provider response conversion failed",
				"trace_id", short, "provider", hit.Provider.Name, "err", convErr)
			httpx.WriteError(w, http.StatusBadGateway, httpx.ErrTypeAPI, fmt.Sprintf("custom provider %q response could not be converted", hit.Provider.Name))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		if _, writeErr := w.Write(anthropicBody); writeErr != nil {
			// The client is gone; ending quietly lets ctx cancellation
			// close the upstream.
			return
		}
		if logPair {
			// The log's input side is the context occupancy, so it is the
			// provider's full prompt count, cache traffic included.
			var usage *observedUsage
			if u != nil {
				usage = &observedUsage{inputTokens: u.PromptTokens, outputTokens: u.CompletionTokens}
			}
			writeCompletionLog(ctx, short, hit, resp.StatusCode, usage, timer, false)
		}
		return
	}

	// Streaming: the OpenAI SSE chunks are converted to Anthropic events as
	// they arrive and written to the client immediately.
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	state := newOAIStreamState(func(event string, data []byte) error {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		// Flush failures only mean the ResponseWriter does not support
		// flushing, in which case writes are already buffered normally.
		_ = rc.Flush()
		return nil
	}, timer)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		line = bytes.TrimSuffix(line, []byte("\r"))
		// Only data: lines carry payloads; event:, comments and other
		// fields are skipped. The one optional space after the colon is part
		// of the wire format.
		if len(line) == 0 || line[0] == ':' || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		if string(data) == "[DONE]" {
			break
		}
		if err := state.Feed(data); err != nil {
			// A write failure means the client disconnected; ending quietly
			// lets ctx cancellation close the upstream.
			return
		}
	}
	// EOF, [DONE] or a read error ends the stream: Finish emits the closing
	// events, or an error event when no finish_reason ever arrived. It is
	// idempotent, so a stream the state machine already terminated (an SSE
	// error payload or a usage-only tail after finish) stays finished.
	if err := state.Finish(); err != nil {
		return
	}
	if err := sc.Err(); err != nil {
		// Mirror Forward: a failed read ends the transfer without a
		// completion line.
		slog.ErrorContext(ctx, "custom provider response read failed",
			"trace_id", short, "provider", hit.Provider.Name, "err", err)
		return
	}
	if logPair {
		var usage *observedUsage
		if u, ok := state.Usage(); ok {
			usage = &observedUsage{inputTokens: u.PromptTokens, outputTokens: u.CompletionTokens}
		}
		writeCompletionLog(ctx, short, hit, resp.StatusCode, usage, timer, true)
	}
}
