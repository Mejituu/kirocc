package customprov

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const mediaRejectBody = `{"error":{"message":"The current model only supports text modality and does not support image input. Please remove the image content from your messages and retry."}}`

const mediaImageRequest = `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`

type mediaUpstreamAttempt struct {
	Path string
	Body []byte
}

type mediaScriptedResponse struct {
	Status      int
	ContentType string
	Header      http.Header
	Body        string
	Chunks      []string
}

// newMediaUpstream serves the scripted responses in order and records every
// request. A request past the script gets a 500, so a missing retry cap shows
// up as an unexpected attempt.
func newMediaUpstream(t *testing.T, responses ...mediaScriptedResponse) (*httptest.Server, func() []mediaUpstreamAttempt) {
	t.Helper()
	var mu sync.Mutex
	var attempts []mediaUpstreamAttempt
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		attempts = append(attempts, mediaUpstreamAttempt{Path: r.URL.Path, Body: body})
		n := len(attempts)
		mu.Unlock()

		resp := mediaScriptedResponse{Status: http.StatusInternalServerError, Body: `{"error":{"message":"unexpected attempt"}}`}
		if n <= len(responses) {
			resp = responses[n-1]
		}
		for key, values := range resp.Header {
			w.Header()[key] = append([]string(nil), values...)
		}
		ct := resp.ContentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		status := resp.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if resp.Chunks != nil {
			f, _ := w.(http.Flusher)
			for _, chunk := range resp.Chunks {
				_, _ = io.WriteString(w, chunk)
				if f != nil {
					f.Flush()
				}
			}
			return
		}
		_, _ = io.WriteString(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []mediaUpstreamAttempt {
		mu.Lock()
		defer mu.Unlock()
		return append([]mediaUpstreamAttempt(nil), attempts...)
	}
}

// newMediaProxy builds one provider/model with the given protocol and media
// flags.
func newMediaProxy(t *testing.T, upstream *httptest.Server, protocol Protocol, textOnly, keepDocuments bool) *Proxy {
	t.Helper()
	model := `{"id":"glm-5.3"`
	if textOnly {
		model += `,"text_only":true`
	}
	if keepDocuments {
		model += `,"keep_documents":true`
	}
	model += `}`
	proto := ""
	if protocol == ProtocolOpenAI {
		proto = `"protocol":"openai",`
	}
	reg, err := loadTestRegistry(t, `{"providers":{"bailian":{"base_url":`+strconv.Quote(upstream.URL)+`,`+
		proto+`"auth_token":"secret-token","models":[`+model+`]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(reg, 5*time.Second)
}

// forwardMedia sends raw through Forward with the request fields peeked from
// the body, like maybeForwardCustom does.
func forwardMedia(t *testing.T, p *Proxy, model, raw string, endpoint Endpoint) *httptest.ResponseRecorder {
	t.Helper()
	var info RequestInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("peek decode: %v", err)
	}
	hit, ok := p.Lookup(model)
	if !ok {
		t.Fatalf("Lookup(%q) missed", model)
	}
	w := httptest.NewRecorder()
	p.Forward(context.Background(), w, http.Header{}, []byte(raw), hit, info, endpoint)
	return w
}

func TestForward_MediaRetrySuccessNonStream(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Header: http.Header{"X-First": {"1"}}, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, Body: `{"ok":true}`},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q, want the retry response", w.Body.String())
	}
	if w.Header().Get("X-First") != "" {
		t.Errorf("first error header leaked: %v", w.Header())
	}
	got := attempts()
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	if !strings.Contains(string(got[0].Body), "aGk=") {
		t.Errorf("first attempt lost the image: %s", got[0].Body)
	}
	if strings.Contains(string(got[1].Body), "aGk=") || !strings.Contains(string(got[1].Body), imageOmittedMarker) {
		t.Errorf("second attempt body = %s, want the image replaced", got[1].Body)
	}
	if !p.media.has("bailian", "glm-5.3") {
		t.Error("successful retry did not record the learned capability")
	}
}

func TestForward_MediaRetrySuccessStream(t *testing.T) {
	const firstChunk = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	const secondChunk = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, ContentType: "text/event-stream", Chunks: []string{firstChunk, secondChunk}},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	raw := strings.Replace(mediaImageRequest, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
	w := forwardMedia(t, p, "claude-glm-5.3", raw, EndpointMessages)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got, want := w.Body.String(), firstChunk+secondChunk; got != want {
		t.Errorf("stream body = %q, want %q", got, want)
	}
	got := attempts()
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	if strings.Contains(string(got[1].Body), "aGk=") || !strings.Contains(string(got[1].Body), imageOmittedMarker) {
		t.Errorf("second attempt body = %s, want the image replaced", got[1].Body)
	}
}

func TestForward_MediaRetryNonMediaErrorPassthrough(t *testing.T) {
	bigBody := `{"error":{"message":"` + strings.Repeat("x", 3000) + `"}}`
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{
			Status: http.StatusBadRequest,
			Header: http.Header{"X-Upstream": {"v"}, "Retry-After": {"7"}},
			Body:   bigBody,
		},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if w.Body.String() != bigBody {
		t.Errorf("body length = %d, want the full %d-byte error body", w.Body.Len(), len(bigBody))
	}
	if w.Header().Get("X-Upstream") != "v" || w.Header().Get("Retry-After") != "7" {
		t.Errorf("error headers not passed through: %v", w.Header())
	}
	if got := attempts(); len(got) != 1 {
		t.Fatalf("attempts = %d, want 1 for a non-media error", len(got))
	}
}

func TestForward_MediaRetryFailureReturnsFirstError(t *testing.T) {
	firstBody := `{"error":{"message":"The current model only supports text modality and does not support image input. ` + strings.Repeat("y", 3000) + `"}}`
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Header: http.Header{"X-First": {"1"}}, Body: firstBody},
		mediaScriptedResponse{Status: http.StatusInternalServerError, Header: http.Header{"X-Second": {"2"}}, Body: `{"error":{"message":"boom"}}`},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the first 400", w.Code)
	}
	if w.Body.String() != firstBody {
		t.Errorf("body length = %d, want the full first error body", w.Body.Len())
	}
	if w.Header().Get("X-First") != "1" || w.Header().Get("X-Second") != "" {
		t.Errorf("headers = %v, want only the first response headers", w.Header())
	}
	if got := attempts(); len(got) != 2 {
		t.Fatalf("attempts = %d, want exactly 2", len(got))
	}
	if p.media.has("bailian", "glm-5.3") {
		t.Error("failed retry learned the capability")
	}
}

func TestForward_MediaRetryNoMediaRequestNotRetried(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	raw := `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	w := forwardMedia(t, p, "claude-glm-5.3", raw, EndpointMessages)

	if w.Code != http.StatusBadRequest || w.Body.String() != mediaRejectBody {
		t.Fatalf("status/body = %d/%q, want the first error", w.Code, w.Body.String())
	}
	if got := attempts(); len(got) != 1 {
		t.Fatalf("attempts = %d, want 1 for a request without media", len(got))
	}
}

func TestForward_MediaRetryStatusGated(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusInternalServerError, Body: mediaRejectBody},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages)

	if w.Code != http.StatusInternalServerError || w.Body.String() != mediaRejectBody {
		t.Fatalf("status/body = %d/%q, want the first 500", w.Code, w.Body.String())
	}
	if got := attempts(); len(got) != 1 {
		t.Fatalf("attempts = %d, want 1 outside the retry status set", len(got))
	}
}

func TestForward_MediaMemorySkipsRetry(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, Body: `{"ok":true}`},
		mediaScriptedResponse{Status: http.StatusOK, Body: `{"ok":true}`},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	if w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages); w.Code != http.StatusOK {
		t.Fatalf("first status = %d", w.Code)
	}
	if w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages); w.Code != http.StatusOK {
		t.Fatalf("second status = %d", w.Code)
	}

	got := attempts()
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3 (retry, then one proactive request)", len(got))
	}
	if strings.Contains(string(got[2].Body), "aGk=") || !strings.Contains(string(got[2].Body), imageOmittedMarker) {
		t.Errorf("learned request body = %s, want the image replaced before sending", got[2].Body)
	}
}

func TestForward_MediaRetryCountTokens(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, Body: `{"input_tokens":42}`},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointCountTokens)

	if w.Code != http.StatusOK || w.Body.String() != `{"input_tokens":42}` {
		t.Fatalf("status/body = %d/%q", w.Code, w.Body.String())
	}
	got := attempts()
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	for _, attempt := range got {
		if attempt.Path != "/v1/messages/count_tokens" {
			t.Errorf("path = %q, want count_tokens", attempt.Path)
		}
	}
	if strings.Contains(string(got[1].Body), "aGk=") {
		t.Errorf("count_tokens retry kept the image: %s", got[1].Body)
	}
}

func TestForward_TextOnlyKeepDocumentsDocumentOnlyNotRetried(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, true, true)
	raw := `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}}]}]}`
	w := forwardMedia(t, p, "claude-glm-5.3", raw, EndpointMessages)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the first 400", w.Code)
	}
	got := attempts()
	if len(got) != 1 {
		t.Fatalf("attempts = %d, want 1: keep_documents cannot change the request", len(got))
	}
	if !strings.Contains(string(got[0].Body), `"type":"document"`) || !strings.Contains(string(got[0].Body), "cGRm") {
		t.Errorf("document was not kept in the forwarded body: %s", got[0].Body)
	}
}

func TestForwardOpenAI_MediaRetrySuccessNonStream(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, Body: oaiChatCompletion},
	)
	p := newMediaProxy(t, upstream, ProtocolOpenAI, false, false)
	w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	got := attempts()
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	if !strings.Contains(string(got[0].Body), "image_url") {
		t.Errorf("first attempt lost the image: %s", got[0].Body)
	}
	if strings.Contains(string(got[1].Body), "image_url") || !strings.Contains(string(got[1].Body), imageOmittedMarker) {
		t.Errorf("second attempt body = %s, want the image replaced", got[1].Body)
	}
	if !p.media.has("bailian", "glm-5.3") {
		t.Error("successful retry did not record the learned capability")
	}
}

func TestForwardOpenAI_MediaRetryFailureReturnsFirstError(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Header: http.Header{"Retry-After": {"7"}}, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusInternalServerError, Body: `{"error":{"message":"boom"}}`},
	)
	p := newMediaProxy(t, upstream, ProtocolOpenAI, false, false)
	w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the first 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "only supports text modality") {
		t.Errorf("body = %s, want the first error message", w.Body.String())
	}
	if w.Header().Get("Retry-After") != "7" {
		t.Errorf("Retry-After = %q, want the first response's value", w.Header().Get("Retry-After"))
	}
	if got := attempts(); len(got) != 2 {
		t.Fatalf("attempts = %d, want exactly 2", len(got))
	}
	if p.media.has("bailian", "glm-5.3") {
		t.Error("failed retry learned the capability")
	}
}

func TestForwardOpenAI_MediaRetrySuccessStream(t *testing.T) {
	chunks := []string{
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, ContentType: "text/event-stream", Chunks: chunks},
	)
	p := newMediaProxy(t, upstream, ProtocolOpenAI, false, false)
	raw := strings.Replace(mediaImageRequest, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
	w := forwardMedia(t, p, "claude-glm-5.3", raw, EndpointMessages)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	for _, want := range []string{"event: message_start", "event: message_stop", "Hello"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
	got := attempts()
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	if strings.Contains(string(got[1].Body), "image_url") {
		t.Errorf("retry kept the image: %s", got[1].Body)
	}
}

func TestForwardOpenAI_MediaMemorySkipsRetry(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, Body: oaiChatCompletion},
		mediaScriptedResponse{Status: http.StatusOK, Body: oaiChatCompletion},
	)
	p := newMediaProxy(t, upstream, ProtocolOpenAI, false, false)
	if w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages); w.Code != http.StatusOK {
		t.Fatalf("first status = %d", w.Code)
	}
	if w := forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages); w.Code != http.StatusOK {
		t.Fatalf("second status = %d", w.Code)
	}

	got := attempts()
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3 (retry, then one proactive request)", len(got))
	}
	if strings.Contains(string(got[2].Body), "image_url") || !strings.Contains(string(got[2].Body), imageOmittedMarker) {
		t.Errorf("learned request body = %s, want the image replaced before sending", got[2].Body)
	}
}

func TestForward_MediaRetryLogsReason(t *testing.T) {
	upstream, _ := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, Body: `{"ok":true}`},
	)
	p := newMediaProxy(t, upstream, ProtocolAnthropic, false, false)
	buf := captureLogs(t, 0)
	_ = forwardMedia(t, p, "claude-glm-5.3", mediaImageRequest, EndpointMessages)

	rec := findLogRecord(t, buf, "custom provider media replaced")
	attrs, _ := rec["attributes"].(map[string]any)
	if attrs["reason"] != "retry" {
		t.Errorf("reason = %v, want retry", attrs["reason"])
	}
	if attrs["images"] != float64(1) || attrs["documents"] != float64(0) {
		t.Errorf("counts = %v/%v, want 1/0", attrs["images"], attrs["documents"])
	}
	if strings.Contains(buf.String(), "aGk=") {
		t.Errorf("image payload leaked into logs:\n%s", buf.String())
	}
}

func TestForward_MediaRetryKeepsWebSearchBlock(t *testing.T) {
	upstream, attempts := newMediaUpstream(t,
		mediaScriptedResponse{Status: http.StatusBadRequest, Body: mediaRejectBody},
		mediaScriptedResponse{Status: http.StatusOK, Body: `{"ok":true}`},
	)
	reg, err := loadTestRegistry(t, `{"providers":{"bailian":{"base_url":`+strconv.Quote(upstream.URL)+`,"auth_token":"secret-token","models":[{"id":"glm-5.3","web_search":true}]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, 5*time.Second)
	raw := strings.Replace(mediaImageRequest, `"max_tokens":16`, `"max_tokens":16,"tools":[{"type":"web_search_20250305","name":"web_search"}]`, 1)
	w := forwardMedia(t, p, "claude-glm-5.3", raw, EndpointMessages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	got := attempts()
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	for _, want := range []string{"x-anthropic-billing-header", "web_search", imageOmittedMarker} {
		if !strings.Contains(string(got[1].Body), want) {
			t.Errorf("retry body missing %q: %s", want, got[1].Body)
		}
	}
}
