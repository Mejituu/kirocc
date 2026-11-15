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

	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/logging"
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
}

// Proxy forwards matched requests to their custom provider. It deliberately
// shares none of the Kiro pipeline: no model resolution, no payload
// conversion, no usage recording.
type Proxy struct {
	registry *Registry
	client   *http.Client
}

// NewProxy builds the forwarding client. responseHeaderTimeout bounds the
// wait for provider response headers, mirroring the Kiro path's
// -response-header-timeout. There is no overall timeout: streaming responses
// run for minutes.
func NewProxy(reg *Registry, responseHeaderTimeout time.Duration) *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &Proxy{registry: reg, client: &http.Client{Transport: transport}}
}

// Lookup reports whether a requested model ID maps to a custom provider.
func (p *Proxy) Lookup(model string) (Hit, bool) {
	return p.registry.Lookup(model)
}

// ListModels returns the custom models for /v1/models.
func (p *Proxy) ListModels() []ModelInfo {
	return p.registry.ListModels()
}

// Forward sends the rewritten request body to the provider and copies its
// response to w verbatim: status, headers and body pass through unchanged,
// streaming responses are flushed as they arrive. ctx is the client request
// context, so a client disconnect cancels the upstream request.
func (p *Proxy) Forward(ctx context.Context, w http.ResponseWriter, h http.Header, raw []byte, hit Hit, info RequestInfo, endpoint Endpoint) {
	_, short := logging.TraceIDs(ctx)
	start := time.Now()
	slog.InfoContext(ctx, "--> POST "+string(endpoint),
		"trace_id", short,
		"provider", hit.Provider.Name,
		"model", info.Model,
		"upstream_model", hit.BaseModel,
		"stream", info.Stream,
	)

	body, err := rewriteModel(raw, hit.BaseModel)
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
		slog.ErrorContext(ctx, "custom provider unreachable",
			"trace_id", short, "provider", hit.Provider.Name, "err", err)
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
	slog.InfoContext(ctx, "<-- POST "+string(endpoint),
		"trace_id", short,
		"provider", hit.Provider.Name,
		"status", resp.StatusCode,
		"duration", time.Since(start),
	)

	// Copy the body as it arrives so SSE events reach the client while the
	// provider is still generating. A write failure means the client
	// disconnected; returning lets ctx cancellation close the upstream.
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			// Flush failures only mean the ResponseWriter does not support
			// flushing, in which case writes are already buffered normally.
			_ = rc.Flush()
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				slog.ErrorContext(ctx, "custom provider response read failed",
					"trace_id", short, "provider", hit.Provider.Name, "err", readErr)
			}
			return
		}
	}
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
