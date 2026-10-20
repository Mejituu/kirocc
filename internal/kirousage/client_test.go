package kirousage

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// usageFixture mirrors the GetUsageLimits fields kirousage decodes: a plan
// allowance with an active free trial, one active and one expired bonus.
const usageFixture = `{
  "nextDateReset": 1793520000,
  "subscriptionInfo": {"subscriptionTitle": "KIRO PRO+"},
  "overageConfiguration": {"overageStatus": "DISABLED"},
  "userInfo": {"email": "dev@example.com"},
  "usageBreakdownList": [
    {
      "currentUsage": 1200,
      "currentUsageWithPrecision": 1200.5,
      "usageLimit": 2000,
      "usageLimitWithPrecision": 2000,
      "currentOverages": 0,
      "overageCharges": 0,
      "currency": "USD",
      "freeTrialInfo": {
        "freeTrialStatus": "ACTIVE",
        "freeTrialExpiry": 1793000000,
        "currentUsage": 34,
        "currentUsageWithPrecision": 34.25,
        "usageLimit": 500,
        "usageLimitWithPrecision": 500
      },
      "bonuses": [
        {"status": "ACTIVE", "currentUsage": 10, "usageLimit": 100},
        {"status": "EXPIRED", "currentUsage": 50, "usageLimit": 50}
      ]
    }
  ]
}`

func TestClient_Get(t *testing.T) {
	var (
		gotTarget string
		gotAuth   string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTarget = r.Header.Get("X-Amz-Target")
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = w.Write([]byte(usageFixture))
	}))
	defer srv.Close()

	l, err := New(WithBaseURL(srv.URL)).Get(context.Background(), Request{
		Token: "tok", Region: "us-east-1", ProfileARN: "arn:aws:codewhisperer:us-east-1:123:profile/ABC",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if gotTarget != amzTarget {
		t.Errorf("X-Amz-Target = %q, want %q", gotTarget, amzTarget)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["profileArn"] != "arn:aws:codewhisperer:us-east-1:123:profile/ABC" || gotBody["resourceType"] != resourceType {
		t.Errorf("request body = %v", gotBody)
	}

	if l.Plan != "KIRO PRO+" || l.Email != "dev@example.com" {
		t.Errorf("plan fields = %+v", l)
	}
	if !l.ResetAt.Equal(time.Unix(1793520000, 0)) {
		t.Errorf("reset = %v", l.ResetAt)
	}
	if l.Base != (Bucket{Used: 1200.5, Limit: 2000}) {
		t.Errorf("base = %+v, want precision values", l.Base)
	}
	if l.FreeTrial == nil || !l.FreeTrial.Active() || l.FreeTrial.Used != 34.25 ||
		!l.FreeTrial.ExpiresAt.Equal(time.Unix(1793000000, 0)) {
		t.Errorf("free trial = %+v", l.FreeTrial)
	}
	if len(l.Bonuses) != 2 || !l.Bonuses[0].Active() || l.Bonuses[1].Active() {
		t.Errorf("bonuses = %+v", l.Bonuses)
	}
	if l.OverageStatus != "DISABLED" {
		t.Errorf("overage status = %q", l.OverageStatus)
	}

	// Totals count the plan plus active grants only; the expired bonus is out.
	if got, want := l.Limit(), 2000.0+500+100; got != want {
		t.Errorf("Limit = %v, want %v", got, want)
	}
	if got, want := l.Used(), 1200.5+34.25+10; got != want {
		t.Errorf("Used = %v, want %v", got, want)
	}
	if got, want := l.Remaining(), 2600-1244.75; got != want {
		t.Errorf("Remaining = %v, want %v", got, want)
	}
}

func TestClient_Get_SparseResponses(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantUsed  float64
		wantLimit float64
	}{
		{"empty object", `{}`, 0, 0},
		{"breakdown without precision values", `{"usageBreakdownList":[{"currentUsage":7,"usageLimit":50}]}`, 7, 50},
		{"over limit floors remaining", `{"usageBreakdownList":[{"currentUsage":60,"usageLimit":50}]}`, 60, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			l, err := New(WithBaseURL(srv.URL)).Get(context.Background(), Request{Token: "t", Region: "us-east-1", ProfileARN: "arn:x"})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if l.Used() != tt.wantUsed || l.Limit() != tt.wantLimit {
				t.Errorf("used/limit = %v/%v, want %v/%v", l.Used(), l.Limit(), tt.wantUsed, tt.wantLimit)
			}
			if l.Remaining() < 0 {
				t.Errorf("Remaining = %v, must not be negative", l.Remaining())
			}
			if !l.ResetAt.IsZero() || l.FreeTrial != nil {
				t.Errorf("absent fields must read as unknown: %+v", l)
			}
		})
	}
}

func TestClient_Get_Errors(t *testing.T) {
	t.Run("missing inputs send nothing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("request must not be sent")
		}))
		defer srv.Close()
		for _, r := range []Request{
			{Region: "us-east-1", ProfileARN: "arn:x"},
			{Token: "t", ProfileARN: "arn:x"},
			{Token: "t", Region: "us-east-1"},
		} {
			if _, err := New(WithBaseURL(srv.URL)).Get(context.Background(), r); err == nil {
				t.Errorf("Get(%+v): want error", r)
			}
		}
	})
	for _, tt := range []struct {
		name, body, want string
		status           int
	}{
		{"access denied surfaces status and body", `{"__type":"AccessDeniedException","message":"Invalid token"}`, "status 403: {\"__type\":\"AccessDeniedException\"", http.StatusForbidden},
		{"server error", `oops`, "status 500", http.StatusInternalServerError},
		{"malformed json", `{"usageBreakdownList":`, "decode response", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := New(WithBaseURL(srv.URL)).Get(context.Background(), Request{Token: "t", Region: "us-east-1", ProfileARN: "arn:x"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
