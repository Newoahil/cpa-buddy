// Package omc is a minimal MCP (Streamable HTTP) client for reading the CPA
// quota that Oh My CPA has already collected.
//
// It speaks JSON-RPC over HTTP POST: initialize -> notifications/initialized ->
// tools/call. The endpoint is stateless and answers in plain JSON, so one
// client instance caches the negotiated protocol version and reuses it; a
// tools/call failure resets the cache so the next attempt re-initializes.
//
// The read-only MCP key is a secret. It is only ever placed in the
// Authorization header: it is never logged, never included in an error, and
// never rendered back to a caller.
package omc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// defaultProtocolVersion is the MCP revision this client offers. OMC's
// go-sdk server supports 2025-03-26 (and newer); it negotiates and answers with
// its own supported version, which the client then reuses.
const defaultProtocolVersion = "2025-03-26"

// maxBodyBytes bounds every response so a hostile or broken endpoint cannot
// exhaust memory.
const maxBodyBytes = 8 << 20

// maxErrorText bounds server-supplied text copied into an error.
const maxErrorText = 200

// QuotaItem is one credential's quota as returned by OMC's quota_list tool.
// The field names mirror OMC's operations.Quota / quota.QuotaWindow JSON.
type QuotaItem struct {
	AuthIndex    string   `json:"auth_index"`
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	Status       string   `json:"status"`
	ObservedAtMS int64    `json:"observed_at_ms"`
	Plan         *Plan    `json:"plan,omitempty"`
	Windows      []Window `json:"windows"`
}

// Plan is OMC's normalized subscription reading.
type Plan struct {
	PlanType           string `json:"plan_type"`
	PlanLabel          string `json:"plan_label"`
	Tier               string `json:"tier"`
	SubscriptionActive *bool  `json:"subscription_active,omitempty"`
}

// Window is one normalized quota window. UsedPercent and RemainingPercent are
// on the 0..100 scale; ResetAtMS is epoch milliseconds; PeriodHours is the
// upstream-reported window length in hours.
type Window struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	Kind             string   `json:"kind,omitempty"`
	Scope            string   `json:"scope"`
	Model            string   `json:"model,omitempty"`
	UsedPercent      *float64 `json:"used_percent,omitempty"`
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	ResetAtMS        *int64   `json:"reset_at_ms,omitempty"`
	ResetLabel       string   `json:"reset_label,omitempty"`
	PeriodHours      *float64 `json:"period_hours,omitempty"`
	ResetAccuracy    string   `json:"reset_accuracy,omitempty"`
}

// Client talks to one OMC MCP endpoint.
type Client struct {
	endpoint string
	key      string
	http     *http.Client

	mu      sync.Mutex
	version string
	nextID  int64
}

// New builds a client. A non-positive timeout falls back to 20s.
func New(endpoint, key string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		endpoint: endpoint,
		key:      strings.TrimSpace(key),
		http:     &http.Client{Timeout: timeout},
	}
}

// ListQuotas calls the read-only quota_list tool and returns its items.
//
// A non-nil error means the call itself failed (transport, protocol, JSON-RPC
// or a tool-level refusal). An empty slice with a nil error means OMC answered
// successfully but has nothing to report, which is not a failure.
func (c *Client) ListQuotas(ctx context.Context) ([]QuotaItem, error) {
	data, err := c.callTool(ctx, "quota_list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []QuotaItem `json:"items"`
	}
	if json.Unmarshal(data, &out) != nil {
		return nil, errors.New("omc: invalid quota_list payload")
	}
	return out.Items, nil
}

// callTool performs initialize (when needed) and one tools/call, returning the
// tool's `data` payload.
func (c *Client) callTool(ctx context.Context, name string, args any) (json.RawMessage, error) {
	if err := c.ensureInitialized(ctx); err != nil {
		return nil, err
	}
	result, err := c.post(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		// A protocol/session failure may mean the endpoint restarted with a
		// different version; drop the cache so the next call re-initializes.
		c.resetProtocol()
		return nil, err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if json.Unmarshal(result, &res) != nil {
		return nil, fmt.Errorf("omc: invalid %s result", name)
	}
	if res.IsError {
		return nil, fmt.Errorf("omc: %s failed: %s", name, bounded(contentText(res.Content)))
	}
	envelope := bytes.TrimSpace(res.StructuredContent)
	if len(envelope) == 0 || bytes.Equal(envelope, []byte("null")) {
		envelope = []byte(contentText(res.Content))
	}
	if len(bytes.TrimSpace(envelope)) == 0 {
		return nil, fmt.Errorf("omc: %s returned no data", name)
	}
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Code   string          `json:"code"`
	}
	if json.Unmarshal(envelope, &env) != nil {
		return nil, fmt.Errorf("omc: invalid %s envelope", name)
	}
	if env.Status == "" && len(bytes.TrimSpace(env.Data)) == 0 {
		// A server that returns the payload directly rather than wrapped.
		return json.RawMessage(envelope), nil
	}
	if env.Status != "" && env.Status != "success" {
		// code is a server-side snake_case identifier, never a secret.
		return nil, fmt.Errorf("omc: %s status %s (%s)", name, bounded(env.Status), bounded(env.Code))
	}
	if len(bytes.TrimSpace(env.Data)) == 0 {
		return nil, fmt.Errorf("omc: %s returned no data", name)
	}
	return env.Data, nil
}

func contentText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	for _, part := range content {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			return part.Text
		}
	}
	return ""
}

// ensureInitialized performs the handshake once per client.
func (c *Client) ensureInitialized(ctx context.Context) error {
	if c.protocol() != "" {
		return nil
	}
	result, err := c.post(ctx, "initialize", map[string]any{
		"protocolVersion": defaultProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cpa-buddy", "version": "1"},
	})
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &init) != nil {
		return errors.New("omc: invalid initialize result")
	}
	version := strings.TrimSpace(init.ProtocolVersion)
	if version == "" {
		version = defaultProtocolVersion
	}
	c.mu.Lock()
	c.version = version
	c.mu.Unlock()
	// The initialized notification carries no id and no JSON-RPC response.
	body, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}{"2.0", "notifications/initialized"})
	if _, status, err := c.do(ctx, body); err != nil {
		return err
	} else if status < 200 || status >= 300 {
		return fmt.Errorf("omc: initialized notification http %d", status)
	}
	return nil
}

// post sends one request and decodes its JSON-RPC result.
func (c *Client) post(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", id, method, mustJSON(params)})
	if err != nil {
		return nil, errors.New("omc: build request failed")
	}
	raw, status, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("omc: http %d", status)
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &msg) != nil || (msg.Result == nil && msg.Error == nil) {
		return nil, errors.New("omc: invalid json-rpc response")
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("omc: json-rpc error %d: %s", msg.Error.Code, bounded(msg.Error.Message))
	}
	return msg.Result, nil
}

// do performs the HTTP round trip. Errors never embed the endpoint or the key.
func (c *Client) do(ctx context.Context, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("omc: invalid request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if v := c.protocol(); v != "" {
		req.Header.Set("MCP-Protocol-Version", v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error string can include the URL (and userinfo); keep
		// the original for errors.Is while exposing only a stable message.
		return nil, 0, errmessage{cause: err}
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if readErr != nil {
		return nil, resp.StatusCode, errors.New("omc: read response failed")
	}
	if len(data) > maxBodyBytes {
		return nil, resp.StatusCode, errors.New("omc: response too large")
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data = extractSSE(data)
	}
	return data, resp.StatusCode, nil
}

func (c *Client) protocol() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

func (c *Client) resetProtocol() {
	c.mu.Lock()
	c.version = ""
	c.mu.Unlock()
}

// errmessage wraps a transport cause so errors.Is still works but the rendered
// message carries no URL from the underlying error.
type errmessage struct{ cause error }

func (e errmessage) Error() string        { return "omc: request failed" }
func (e errmessage) Unwrap() error        { return e.cause }
func (e errmessage) Is(target error) bool { return errors.Is(e.cause, target) }

// extractSSE returns the last JSON-RPC message payload in an SSE body. OMC
// answers in JSON, but the Accept header permits an event stream and a caller
// must not choke on one.
func extractSSE(raw []byte) []byte {
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		if bytes.Contains(payload, []byte(`"result"`)) || bytes.Contains(payload, []byte(`"error"`)) {
			last = payload
		}
	}
	return last
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}

func bounded(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrorText {
		s = s[:maxErrorText] + "…"
	}
	return s
}
