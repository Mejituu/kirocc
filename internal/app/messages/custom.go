package messages

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/customprov"
	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/models"
	"github.com/d-kuro/kirocc/internal/reqconv"
	"github.com/d-kuro/kirocc/internal/timing"
	"github.com/d-kuro/kirocc/internal/tokencount"
)

// maybeForwardCustom routes a request to a custom provider when its model
// matches one declared in the custom provider config. It returns true when
// the request was fully handled; otherwise the request body is restored on r
// and the caller proceeds down the regular Kiro path.
//
// The body is read up front because the custom path forwards the original
// bytes — decoding into anthropic.Request would drop fields the provider
// understands. Reading under the same MaxBytesReader cap keeps the
// over-limit response identical to the regular path's.
func (s *Service) maybeForwardCustom(ctx context.Context, w http.ResponseWriter, r *http.Request, endpoint customprov.Endpoint) bool {
	if s.custom == nil {
		return false
	}
	// HandleMessages starts the timing record before this call; count_tokens
	// reaches here without one, so start it here (Start reuses when present).
	ctx, _ = timing.Start(ctx)
	traceID, short := logging.TraceIDs(ctx)

	raw, err := readRawRequestBody(w, r, s.maxRequestBody)
	if err != nil {
		// HandleMessages distinguishes an over-cap body (413, with a warning
		// log) from a malformed one (400); HandleCountTokens answers 400 for
		// both and logs nothing. Keep each endpoint's error behavior exactly
		// as the regular path has it.
		if endpoint == customprov.EndpointMessages {
			if errors.As(err, new(*http.MaxBytesError)) {
				slog.WarnContext(ctx, "request body too large",
					"trace_id", short, "limit_bytes", s.maxRequestBody, "content_length", r.ContentLength)
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, errTypeInvalidRequest,
					fmt.Sprintf("request body exceeds the %d byte limit; raise -max-request-body (or KIROCC_MAX_REQUEST_BODY) to allow larger requests", s.maxRequestBody))
				return true
			}
			slog.WarnContext(ctx, "invalid request", "trace_id", short, "err", err)
		}
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		return true
	}

	// The Claude Code session ID rides along for the logs and the
	// session-following mappings; unlike the Kiro path nothing is generated
	// when the header is absent.
	ccSessionID := r.Header.Get(headerCCSessionID)
	if ccSessionID != "" {
		ctx = logging.WithSessionID(ctx, ccSessionID)
	}

	var peek customprov.RequestInfo
	if err := json.Unmarshal(raw, &peek); err != nil {
		// Unparseable body: not a custom-model request; the regular path
		// reports the decode error.
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return false
	}

	// Session-following mappings apply only to /v1/messages and only when
	// the session is known. A mapped request never updates the session's
	// provider — not even when its target turns out unknown and the request
	// falls back to normal routing.
	recordSession := endpoint == customprov.EndpointMessages && ccSessionID != ""
	if recordSession {
		if sessionProvider, mapping, ok := s.custom.MatchMapping(ccSessionID, peek); ok {
			recordSession = false
			slog.InfoContext(ctx, "model mapped",
				"trace_id", short,
				"session_id", logging.ShortID(ccSessionID),
				"session_provider", sessionProvider,
				"from", peek.Model,
				"to_provider", mapping.Provider,
				"to_model", mapping.ModelTo,
			)
			if mapping.Provider == customprov.KiroProviderName {
				if _, _, known := models.ResolveKnown(mapping.ModelTo); known {
					newRaw, err := customprov.RewriteModel(raw, mapping.ModelTo)
					if err != nil {
						// The model field was peeked as a string, so the
						// rewrite cannot fail; treat the request as unmapped
						// rather than forward a wrong body.
						slog.ErrorContext(ctx, "model mapping rewrite failed", "trace_id", short, "err", err)
					} else {
						// Hand the rewritten body to the Kiro path, whose own
						// logs and resolution then show the target model.
						r.Body = io.NopCloser(bytes.NewReader(newRaw))
						r.ContentLength = int64(len(newRaw))
						return false
					}
				} else {
					slog.WarnContext(ctx, "model mapping target unknown", "trace_id", short, "to_model", mapping.ModelTo)
				}
			} else if hit, ok := s.custom.LookupModel(mapping.Provider, mapping.ModelTo); ok {
				debugRequestLogs(ctx, traceID, endpoint, r, raw)
				s.custom.Forward(ctx, w, r.Header, raw, hit, peek, endpoint)
				return true
			} else {
				// Rare: reachable only when a config reload lands between
				// the mapping match above and this target lookup, removing
				// the target. Treated like an unknown Kiro target.
				slog.WarnContext(ctx, "model mapping target unknown", "trace_id", short, "to_model", mapping.ModelTo)
			}
		}
	}

	hit, ok := s.custom.Lookup(peek.Model)
	if !ok {
		// A Kiro-routed (or unknown) model with a web_search request falls
		// back to the configured web-search target when the kiro section
		// names one. Like the mapping path, the fallback request does not
		// update the session's provider; count_tokens never falls back.
		if endpoint == customprov.EndpointMessages && peek.HasWebSearch() {
			if fallbackHit, ok := s.custom.KiroWebSearchFallbackHit(); ok {
				slog.InfoContext(ctx, "web search fallback",
					"trace_id", short,
					"session_id", logging.ShortID(ccSessionID),
					"from_provider", customprov.KiroProviderName,
					"from_model", peek.Model,
					"to_provider", fallbackHit.Provider.Name,
					"to_model", fallbackHit.BaseModel,
				)
				debugRequestLogs(ctx, traceID, endpoint, r, raw)
				s.custom.Forward(ctx, w, r.Header, raw, fallbackHit, peek, endpoint)
				return true
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if recordSession {
			s.custom.RecordSession(ccSessionID, customprov.KiroProviderName)
		}
		return false
	}

	// A custom model without native web_search redirects a web_search request
	// to its provider's fallback target. Like the mapping path this records
	// no session and skips the local count_tokens estimate: the request
	// forwards as the fallback model.
	if !hit.WebSearch && endpoint == customprov.EndpointMessages && peek.HasWebSearch() {
		if fallbackHit, ok := s.custom.WebSearchFallbackHit(hit.Provider.Name); ok {
			slog.InfoContext(ctx, "web search fallback",
				"trace_id", short,
				"session_id", logging.ShortID(ccSessionID),
				"from_provider", hit.Provider.Name,
				"from_model", hit.BaseModel,
				"to_provider", fallbackHit.Provider.Name,
				"to_model", fallbackHit.BaseModel,
			)
			debugRequestLogs(ctx, traceID, endpoint, r, raw)
			s.custom.Forward(ctx, w, r.Header, raw, fallbackHit, peek, endpoint)
			return true
		}
	}

	debugRequestLogs(ctx, traceID, endpoint, r, raw)

	// count_tokens stays local when the provider is configured without it:
	// providers without a count_tokens endpoint would otherwise turn every
	// probe into an upstream error.
	if endpoint == customprov.EndpointCountTokens && !hit.Provider.CountTokens {
		s.countTokensLocally(ctx, w, r, raw, hit)
		return true
	}

	if recordSession {
		s.custom.RecordSession(ccSessionID, hit.Provider.Name)
	}
	s.custom.Forward(ctx, w, r.Header, raw, hit, peek, endpoint)
	return true
}

// debugRequestLogs emits the custom path's debug request logging, mirroring
// the Kiro path: the body as parseAndValidateRequest logs it, then the
// headers as HandleMessages logs them — the latter only on /v1/messages,
// whose Kiro counterpart logs them (HandleCountTokens does not).
func debugRequestLogs(ctx context.Context, traceID string, endpoint customprov.Endpoint, r *http.Request, raw []byte) {
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.DebugContext(ctx, "client request body", "request_body", jsontext.Value(raw))
	}
	if endpoint == customprov.EndpointMessages {
		slog.DebugContext(ctx, "client request headers",
			"trace_id", traceID,
			"session_id", logging.SessionIDFromContext(ctx),
			"headers", logging.SafeHeaders{H: r.Header},
		)
	}
}

// readRawRequestBody reads the whole body under the size cap, wrapping read
// errors the way parseAndValidateRequest does.
func readRawRequestBody(w http.ResponseWriter, r *http.Request, maxBody int64) ([]byte, error) {
	if maxBody > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	return raw, nil
}

// countTokensLocally answers count_tokens for a custom model with the local
// tiktoken estimate. It mirrors HandleCountTokens minus the Kiro parts: the
// payload is built directly with the provider's model ID, so the count covers
// the same tool and effort fields as a Kiro estimate. The raw bytes are
// already in hand and within the size cap, so the request is decoded directly
// rather than re-fed through parseAndValidateRequest's MaxBytesReader.
func (s *Service) countTokensLocally(ctx context.Context, w http.ResponseWriter, r *http.Request, raw []byte, hit customprov.Hit) {
	var req anthropic.Request
	if err := json.UnmarshalRead(bytes.NewReader(raw), &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		return
	}
	if len(req.Messages) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, "messages must not be empty")
		return
	}

	ccSessionID := r.Header.Get(headerCCSessionID)

	thinking := req.IsThinkingEnabled()
	effort := resolveEffort(ctx, hit.BaseModel, &req, thinking)

	payload, _, err := reqconv.BuildPayload(&req, reqconv.BuildOptions{
		ModelID:        hit.BaseModel,
		ConversationID: ccSessionID,
		Effort:         effort,
	})
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		return
	}

	n, err := tokencount.CountPayload(payload)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, errTypeAPI, "token counting unavailable")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, map[string]int{"input_tokens": n}); err != nil {
		slog.ErrorContext(ctx, "write count_tokens response failed", "err", err)
		return
	}
	_, _ = w.Write([]byte("\n"))
}
