package customprov

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/models"
	"github.com/d-kuro/kirocc/internal/timing"
)

// Endpoint selects the provider API a matched request is forwarded to.
type Endpoint string

const (
	EndpointMessages    Endpoint = "/v1/messages"
	EndpointCountTokens Endpoint = "/v1/messages/count_tokens"
)

// RequestInfo carries the request fields peeked before the routing decision;
// forwarding needs no more than these.
type RequestInfo struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`

	// Tools carries the request's tool definitions, decoded by the same peek;
	// only the web_search server-side tool is inspected.
	Tools []anthropic.Tool `json:"tools,omitempty"`

	// Thinking and OutputConfig are peeked only for the request log; the
	// forwarded bytes are untouched. Only the string members kirocc logs
	// are declared, so other fields (budget_tokens, …) decode fine — a
	// decode failure would misroute the request to the Kiro path.
	Thinking     *thinkingPeek     `json:"thinking,omitempty"`
	OutputConfig *outputConfigPeek `json:"output_config,omitempty"`
}

// thinkingPeek mirrors the request's thinking field for the log.
type thinkingPeek struct {
	Type string `json:"type"`
}

// outputConfigPeek mirrors the request's output_config field for the log.
type outputConfigPeek struct {
	Effort string `json:"effort"`
}

// HasWebSearch reports whether the request declares the web_search
// server-side tool.
func (info RequestInfo) HasWebSearch() bool {
	return anthropic.FindWebSearchTool(info.Tools) != nil
}

// Proxy forwards matched requests to their custom provider. It deliberately
// shares none of the Kiro pipeline: no model resolution, no payload
// conversion, no usage recording.
type Proxy struct {
	registry *Registry
	client   *http.Client
	// sessions tracks which provider each Claude Code session last used, for
	// the session-following mappings.
	sessions *sessionTable
}

// NewProxy builds the forwarding client. responseHeaderTimeout bounds the
// wait for provider response headers, mirroring the Kiro path's
// -response-header-timeout. There is no overall timeout: streaming responses
// run for minutes.
func NewProxy(reg *Registry, responseHeaderTimeout time.Duration) *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &Proxy{registry: reg, client: &http.Client{Transport: transport}, sessions: newSessionTable(sessionTableCapacity)}
}

// Lookup reports whether a requested model ID maps to a custom provider.
func (p *Proxy) Lookup(model string) (Hit, bool) {
	return p.registry.Lookup(model)
}

// LookupModel resolves a mapping target: the model with exactly this id in
// the named provider.
func (p *Proxy) LookupModel(providerName, id string) (Hit, bool) {
	return p.registry.lookupModel(providerName, id)
}

// MatchMapping resolves the session-following mapping for a request: the
// session's provider comes from the session table, then the first matching
// rule of that provider's group wins. The session provider is returned for
// logging.
func (p *Proxy) MatchMapping(sessionID string, info RequestInfo) (sessionProvider string, m Mapping, ok bool) {
	provider, ok := p.sessions.get(sessionID)
	if !ok {
		return "", Mapping{}, false
	}
	m, matched := p.registry.matchMapping(provider, info)
	if !matched {
		return "", Mapping{}, false
	}
	return provider, m, true
}

// RecordSession notes which provider a session's most recent unmapped
// request used.
func (p *Proxy) RecordSession(sessionID, provider string) {
	p.sessions.put(sessionID, provider)
}

// ListModels returns the custom models for /v1/models.
func (p *Proxy) ListModels() []ModelInfo {
	return p.registry.ListModels()
}

// Forward sends the rewritten request body to the provider and copies its
// response to w verbatim: status, headers and body pass through unchanged,
// streaming responses are flushed as they arrive. ctx is the client request
// context, so a client disconnect cancels the upstream request.
//
// Logging mirrors the Kiro path: a "--> POST /v1/messages" /
// "<-- POST /v1/messages" pair for the messages endpoint (count_tokens logs
// nothing at the default level, like HandleCountTokens), with the response
// line only after the body is fully read, and a "custom provider error"
// warning when the provider answered with an error status.
func (p *Proxy) Forward(ctx context.Context, w http.ResponseWriter, h http.Header, raw []byte, hit Hit, info RequestInfo, endpoint Endpoint) {
	if hit.Provider.Protocol == ProtocolOpenAI {
		p.forwardOpenAI(ctx, w, h, raw, hit, info, endpoint)
		return
	}
	traceID, short := logging.TraceIDs(ctx)
	// The timer starts when kirocc received the request — maybeForwardCustom
	// put it in ctx (creating it for count_tokens, whose callers do not start
	// one). Forward is also called directly in tests, hence the fallback.
	timer := timing.From(ctx)
	if timer == nil {
		ctx, timer = timing.Start(ctx)
	}
	logPair := endpoint == EndpointMessages
	if logPair {
		slog.InfoContext(ctx, "--> POST /v1/messages", requestLogArgs(ctx, short, hit, info)...)
	}

	// A1 trigger: web search runs natively on the provider only when the
	// model opted in (web_search in the config) and the request declares the
	// tool. RewriteForward then swaps the per-request billing block for the
	// stable fixed one so the provider's prompt-prefix cache survives.
	ws := info.HasWebSearch() && hit.WebSearch
	if ws {
		slog.InfoContext(ctx, "web search enabled", "trace_id", short, "provider", hit.Provider.Name, "mode", "native")
	}
	body, err := RewriteForward(raw, hit.BaseModel, ws)
	if err != nil {
		slog.ErrorContext(ctx, "custom provider request rewrite failed", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, httpx.ErrTypeInvalidRequest, "invalid request: "+err.Error())
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hit.Provider.BaseURL+string(endpoint), bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "custom provider request build failed", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadGateway, httpx.ErrTypeAPI, fmt.Sprintf("custom provider %q request could not be built", hit.Provider.Name))
		return
	}
	copyForwardHeaders(req.Header, h)
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

	for key, values := range resp.Header {
		if hopByHopHeaders[key] || key == "Set-Cookie" {
			continue
		}
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(resp.StatusCode)
	if logPair {
		slog.DebugContext(ctx, "client response headers",
			"trace_id", traceID,
			"session_id", logging.SessionIDFromContext(ctx),
			"headers", logging.SafeHeaders{H: w.Header()},
		)
	}

	// Usage scanning rides alongside the forwarding as a read-only side
	// channel, selected by the response's media type: an event stream's
	// data lines are parsed as they pass, a JSON body is cached whole and
	// parsed at EOF. Only a success-status JSON body is cached — an error
	// keeps the small capture for the warning below. count_tokens logs no
	// completion line, so it does none of this work.
	var scanner *scanUsage
	var jsonBody []byte
	if logPair {
		switch mediaType(resp.Header.Get("Content-Type")) {
		case "text/event-stream":
			scanner = &scanUsage{timer: timer}
		case "application/json":
			if resp.StatusCode < 400 {
				jsonBody = []byte{}
			}
		}
	}

	// Copy the body as it arrives so SSE events reach the client while the
	// provider is still generating. A write failure means the client
	// disconnected; returning lets ctx cancellation close the upstream.
	// An error status also caches the first errBodyLimit bytes for the
	// warning below, while the full body still goes to the client.
	var errBody []byte
	if resp.StatusCode >= 400 {
		errBody = make([]byte, 0, errBodyLimit)
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	readFailed := false
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if errBody != nil && len(errBody) < errBodyLimit {
				errBody = append(errBody, buf[:min(n, errBodyLimit-len(errBody))]...)
			}
			if scanner != nil {
				scanner.feed(buf[:n])
			}
			if jsonBody != nil {
				jsonBody = append(jsonBody, buf[:n]...)
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			// Flush failures only mean the ResponseWriter does not support
			// flushing, in which case writes are already buffered normally.
			_ = rc.Flush()
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				readFailed = true
				slog.ErrorContext(ctx, "custom provider response read failed",
					"trace_id", short, "provider", hit.Provider.Name, "err", readErr)
			}
			break
		}
	}

	// The completion line covers the full body transfer; a read error ends
	// the transfer without one, like the Kiro path's silent stream errors.
	// Usage is reported in the line when the response carried it — an error
	// response generally does not, and then the three fields are simply
	// absent.
	if logPair && !readFailed {
		var usage *observedUsage
		if scanner != nil && scanner.found {
			usage = &scanner.usage
		} else if jsonBody != nil {
			usage = jsonUsage(jsonBody)
		}
		writeCompletionLog(ctx, short, hit, resp.StatusCode, usage, timer, scanner != nil)
	}
	if errBody != nil {
		args := appendSessionID(ctx, []any{"trace_id", short})
		args = append(args, "provider", hit.Provider.Name, "status", resp.StatusCode, "body", string(errBody))
		slog.WarnContext(ctx, "custom provider error", args...)
	}
}

// writeCompletionLog emits the "<-- POST /v1/messages" line both protocol
// paths share: the usage fields when the response carried them, the timing
// fields (streaming selects the speed arguments) and the context-limit
// warning.
func writeCompletionLog(ctx context.Context, short string, hit Hit, status int, usage *observedUsage, timer *timing.Request, streaming bool) {
	args := appendSessionID(ctx, []any{"trace_id", short})
	args = append(args, "provider", hit.Provider.Name, "status", status)
	var pct float64
	var contextUsage string
	var outTokens int
	if usage != nil {
		pct = float64(usage.inputTokens) * 100 / float64(contextWindow(hit))
		contextUsage = fmt.Sprintf("%.1fk(%.1f%%)", float64(usage.inputTokens)/1000, pct)
		outTokens = usage.outputTokens
		args = append(args, "input_tokens", usage.inputTokens, "output_tokens", usage.outputTokens, "context_usage", contextUsage)
	}
	// An error response carries no meaningful speed signal, so it logs
	// only the duration.
	now := time.Now()
	if status < 400 {
		args = append(args, timer.LogArgs(outTokens, now, streaming)...)
	} else {
		args = append(args, timer.DurationArgs(now)...)
	}
	slog.InfoContext(ctx, "<-- POST /v1/messages", args...)
	// Mirror the Kiro path's warning when the context window is full.
	if pct >= 100 {
		slog.WarnContext(ctx, "context limit exceeded", "trace_id", short, "context_usage", contextUsage)
	}
}

// errBodyLimit bounds the response body cached for the error warning.
const errBodyLimit = 2048

// mediaType extracts the media type of a Content-Type header, before any
// parameters, so "text/event-stream; charset=utf-8" still selects the SSE
// scanner.
func mediaType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(base)
}

// requestLogArgs builds the "--> POST /v1/messages" attributes, mirroring the
// Kiro path's logRequest fields: shortened trace and session IDs, the
// requested and upstream model, the thinking label, the stream flag and the
// advertised context window.
func requestLogArgs(ctx context.Context, short string, hit Hit, info RequestInfo) []any {
	args := appendSessionID(ctx, []any{"trace_id", short})
	args = append(args,
		"provider", hit.Provider.Name,
		"model", info.Model,
		"upstream_model", hit.BaseModel,
		"thinking", thinkingLogValue(info),
		"stream", info.Stream,
		"context_window", formatContextWindow(contextWindow(hit)),
	)
	return args
}

// thinkingLogValue mirrors the Kiro path's logRequest label: the requested
// effort level when reasoning is on, "enabled" without one, false otherwise.
// The [1m] suffix never implies thinking here — for a custom model it is only
// a context-window advertisement.
func thinkingLogValue(info RequestInfo) any {
	if info.Thinking == nil ||
		(info.Thinking.Type != anthropic.ThinkingTypeEnabled && info.Thinking.Type != anthropic.ThinkingTypeAdaptive) {
		return false
	}
	if info.OutputConfig != nil && info.OutputConfig.Effort != "" {
		return info.OutputConfig.Effort
	}
	return "enabled"
}

// contextWindow returns the window the model advertises: 1M when the
// configured name carries the [1m] marker — regardless of how the request
// spelled the ID — and the default otherwise.
func contextWindow(hit Hit) int {
	if hit.Config1M {
		return models.ThinkingContextWindowSize
	}
	return models.DefaultContextWindowSize
}

// formatContextWindow renders a context window size as a human label like
// "200k" or "1M", mirroring the Kiro path's log label.
func formatContextWindow(size int) string {
	if size >= 1_000_000 {
		return fmt.Sprintf("%dM", size/1_000_000)
	}
	return fmt.Sprintf("%dk", size/1000)
}

// appendSessionID adds the shortened session ID attribute when the request
// carried one; the Kiro path invents a UUID, the custom path simply omits
// the field.
func appendSessionID(ctx context.Context, args []any) []any {
	if session := logging.SessionIDFromContext(ctx); session != "" {
		args = append(args, "session_id", logging.ShortID(session))
	}
	return args
}

// forwardedRequestHeaders is the whitelist copied from the client request to
// the provider. Credentials are deliberately absent: the provider is
// authenticated with the configured auth_token, not the client's key.
var forwardedRequestHeaders = []string{"Content-Type", "Accept", "User-Agent"}

// copyForwardHeaders copies the whitelisted and all Anthropic-* headers (e.g.
// anthropic-version, anthropic-beta) from the client request to the provider
// request. Header keys in http.Header are canonical, so the prefix match sees
// "Anthropic-Version" and friends.
func copyForwardHeaders(dst, src http.Header) {
	for _, name := range forwardedRequestHeaders {
		if values, ok := src[name]; ok {
			dst[name] = append([]string(nil), values...)
		}
	}
	for name, values := range src {
		if strings.HasPrefix(name, "Anthropic-") {
			dst[name] = append([]string(nil), values...)
		}
	}
}

// hopByHopHeaders are connection-scoped headers that must not be copied from
// the provider response to the client.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}
