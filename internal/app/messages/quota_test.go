package messages

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/kiroclient"
)

func TestUpstreamCallError(t *testing.T) {
	s := New(nil, nil)
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
		wantMsg    string
	}{
		{
			name:       "monthly quota becomes billing_error",
			err:        &kiroclient.UpstreamError{Status: http.StatusTooManyRequests, Exception: "ThrottlingException", Body: `{"reason":"MONTHLY_REQUEST_COUNT"}`},
			wantStatus: http.StatusPaymentRequired,
			wantType:   errTypeBilling,
			wantMsg:    "quota exhausted (MONTHLY_REQUEST_COUNT)",
		},
		{
			name:       "bare 402 becomes billing_error",
			err:        &kiroclient.UpstreamError{Status: http.StatusPaymentRequired},
			wantStatus: http.StatusPaymentRequired,
			wantType:   errTypeBilling,
			wantMsg:    "quota exhausted",
		},
		{
			name:       "plain throttling stays a generic upstream error",
			err:        &kiroclient.UpstreamError{Status: http.StatusTooManyRequests, Exception: "ThrottlingException", Body: `{"__type":"ThrottlingException"}`},
			wantStatus: http.StatusBadGateway,
			wantType:   errTypeAPI,
			wantMsg:    "upstream API error",
		},
		{
			name:       "transport error",
			err:        errors.New("dial tcp: refused"),
			wantStatus: http.StatusBadGateway,
			wantType:   errTypeAPI,
			wantMsg:    "upstream API error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.upstreamCallError(t.Context(), tt.err)
			if got.status != tt.wantStatus || got.jsonType != tt.wantType || got.sseType != tt.wantType {
				t.Fatalf("got %+v, want status=%d type=%s", got, tt.wantStatus, tt.wantType)
			}
			if !strings.Contains(got.jsonMessage, tt.wantMsg) || got.sseMessage != got.jsonMessage {
				t.Fatalf("message = %q / %q, want containing %q", got.jsonMessage, got.sseMessage, tt.wantMsg)
			}
		})
	}
}
