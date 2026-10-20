// Package kirousage fetches the Kiro account's plan and credit usage via
// GetUsageLimits and keeps per-process consumption totals, so kirocc can report
// both at startup, per request and on shutdown.
//
// GetUsageLimits lives on the same control plane as ListAvailableModels
// (management.<region>.kiro.dev, AmazonCodeWhispererService, awsJson1_0).
package kirousage

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	amzTarget = "AmazonCodeWhispererService.GetUsageLimits"
	// resourceType selects the agentic (chat) credit pool, the one kirocc spends.
	resourceType = "AGENTIC_REQUEST"
	// errBodyLimit bounds how much of an error response is read into a message.
	errBodyLimit = 2048
	// respBodyLimit bounds the usage response; the real one is a few KB.
	respBodyLimit = 1 << 20
)

// Request carries the credential and routing inputs for a usage fetch.
type Request struct {
	Token string
	// Region selects the control-plane host; a token is rejected outside the
	// region that issued it.
	Region string
	// ProfileARN scopes the query to the account's CodeWhisperer profile.
	// API-key credentials have none and therefore cannot fetch.
	ProfileARN string
}

// Client fetches usage limits.
type Client struct {
	httpClient *http.Client
	baseURL    string // override for tests; empty = region-based URL
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets a custom endpoint (for testing).
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = url }
}

// New creates a Client.
func New(opts ...Option) *Client {
	c := &Client{httpClient: &http.Client{Timeout: 15 * time.Second}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) endpointURL(region string) string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return fmt.Sprintf("https://management.%s.kiro.dev/", region)
}

// Get fetches the account's usage limits.
func (c *Client) Get(ctx context.Context, r Request) (*Limits, error) {
	if r.Token == "" {
		return nil, fmt.Errorf("kirousage: missing token")
	}
	if r.Region == "" {
		return nil, fmt.Errorf("kirousage: missing region")
	}
	if r.ProfileARN == "" {
		return nil, fmt.Errorf("kirousage: missing profile ARN")
	}

	body, err := json.Marshal(map[string]any{
		"profileArn":      r.ProfileARN,
		"resourceType":    resourceType,
		"isEmailRequired": true,
	})
	if err != nil {
		return nil, fmt.Errorf("kirousage: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(r.Region), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kirousage: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Amz-Target", amzTarget)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kirousage: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return nil, fmt.Errorf("kirousage: status %d: %s", resp.StatusCode, errBody)
	}

	var parsed wireResponse
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, respBodyLimit), &parsed); err != nil {
		return nil, fmt.Errorf("kirousage: decode response: %w", err)
	}
	return parsed.toLimits(), nil
}

// wireResponse mirrors the GetUsageLimits response, limited to fields whose
// name and type are known from kiro-cli's service model or kiro2cc-proxy: a
// guessed field with the wrong JSON type would fail the whole decode. Every
// field is optional, since the shape differs between account types.
type wireResponse struct {
	NextDateReset    *float64 `json:"nextDateReset"`
	SubscriptionInfo *struct {
		SubscriptionTitle string `json:"subscriptionTitle"`
	} `json:"subscriptionInfo"`
	OverageConfiguration *struct {
		OverageStatus string `json:"overageStatus"`
	} `json:"overageConfiguration"`
	UserInfo *struct {
		Email string `json:"email"`
	} `json:"userInfo"`
	UsageBreakdownList []wireBreakdown `json:"usageBreakdownList"`
}

type wireBreakdown struct {
	CurrentUsage              float64  `json:"currentUsage"`
	CurrentUsageWithPrecision *float64 `json:"currentUsageWithPrecision"`
	UsageLimit                float64  `json:"usageLimit"`
	UsageLimitWithPrecision   *float64 `json:"usageLimitWithPrecision"`
	CurrentOverages           float64  `json:"currentOverages"`
	OverageCharges            float64  `json:"overageCharges"`
	Currency                  string   `json:"currency"`
	NextDateReset             *float64 `json:"nextDateReset"`
	FreeTrialInfo             *struct {
		FreeTrialStatus           string   `json:"freeTrialStatus"`
		FreeTrialExpiry           *float64 `json:"freeTrialExpiry"`
		CurrentUsage              float64  `json:"currentUsage"`
		CurrentUsageWithPrecision *float64 `json:"currentUsageWithPrecision"`
		UsageLimit                float64  `json:"usageLimit"`
		UsageLimitWithPrecision   *float64 `json:"usageLimitWithPrecision"`
	} `json:"freeTrialInfo"`
	Bonuses []struct {
		Status       string  `json:"status"`
		CurrentUsage float64 `json:"currentUsage"`
		UsageLimit   float64 `json:"usageLimit"`
	} `json:"bonuses"`
}

func (w *wireResponse) toLimits() *Limits {
	l := &Limits{}
	if w.SubscriptionInfo != nil {
		l.Plan = w.SubscriptionInfo.SubscriptionTitle
	}
	if w.OverageConfiguration != nil {
		l.OverageStatus = w.OverageConfiguration.OverageStatus
	}
	if w.UserInfo != nil {
		l.Email = w.UserInfo.Email
	}
	l.ResetAt = epoch(w.NextDateReset)

	// The request asks for a single resource type, so the first breakdown is
	// the agentic-request pool.
	if len(w.UsageBreakdownList) == 0 {
		return l
	}
	b := &w.UsageBreakdownList[0]
	l.Base = Bucket{
		Used:  precise(b.CurrentUsageWithPrecision, b.CurrentUsage),
		Limit: precise(b.UsageLimitWithPrecision, b.UsageLimit),
	}
	l.Overages = b.CurrentOverages
	l.OverageCharges = b.OverageCharges
	l.Currency = b.Currency
	if l.ResetAt.IsZero() {
		l.ResetAt = epoch(b.NextDateReset)
	}
	if t := b.FreeTrialInfo; t != nil {
		l.FreeTrial = &Grant{
			Status:    t.FreeTrialStatus,
			Used:      precise(t.CurrentUsageWithPrecision, t.CurrentUsage),
			Limit:     precise(t.UsageLimitWithPrecision, t.UsageLimit),
			ExpiresAt: epoch(t.FreeTrialExpiry),
		}
	}
	for _, bonus := range b.Bonuses {
		l.Bonuses = append(l.Bonuses, Grant{
			Status: bonus.Status,
			Used:   bonus.CurrentUsage,
			Limit:  bonus.UsageLimit,
		})
	}
	return l
}

// precise prefers the fractional "WithPrecision" variant when present.
func precise(withPrecision *float64, whole float64) float64 {
	if withPrecision != nil {
		return *withPrecision
	}
	return whole
}

// epoch converts an awsJson epoch-seconds timestamp; nil or zero is "unknown".
func epoch(sec *float64) time.Time {
	if sec == nil || *sec <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(*sec), 0)
}
