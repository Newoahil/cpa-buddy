package omc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rpcRequestBody is the subset of a JSON-RPC request the fixture inspects.
type rpcRequestBody struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// newMCPServer builds a stateless OMC-like MCP fixture: it answers initialize,
// accepts notifications, and routes tools/call to callResponse.
func newMCPServer(t *testing.T, version string, callResponse func(body []byte) (status int, payload string)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var initializes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer read-only-key" {
			t.Errorf("missing/incorrect bearer: %q", r.Header.Get("Authorization"))
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/json") {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		raw, _ := io.ReadAll(r.Body)
		var req rpcRequestBody
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("bad request json: %v (%s)", err, raw)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			initializes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"` + version + `","capabilities":{},"serverInfo":{"name":"oh-my-cpa","version":"test"}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if r.Header.Get("MCP-Protocol-Version") == "" {
				t.Errorf("tools/call must carry the negotiated MCP-Protocol-Version header")
			}
			status, payload := callResponse(raw)
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(payload))
		default:
			t.Errorf("unexpected method %q", req.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &initializes
}

func quotaListPayload() string {
	return `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"status\":\"success\"}"}],"structuredContent":{"status":"success","data":{"items":[{"auth_index":"a1","name":"work","provider":"codex","status":"warning","observed_at_ms":1700000000000,"plan":{"plan_type":"plus","plan_label":"Plus","tier":"premium"},"windows":[{"id":"five_hour","label":"5小时用量上限 (5-Hour)","kind":"five_hour","scope":"standard","used_percent":42.5,"reset_at_ms":1700003600000,"period_hours":5,"reset_accuracy":"exact"}]}]}},"isError":false}}`
}

func TestListQuotasHappyPath(t *testing.T) {
	srv, initializes := newMCPServer(t, "2025-03-26", func([]byte) (int, string) {
		return http.StatusOK, quotaListPayload()
	})
	c := New(srv.URL, "read-only-key", 5*time.Second)
	items, err := c.ListQuotas(context.Background())
	if err != nil {
		t.Fatalf("ListQuotas: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	got := items[0]
	if got.AuthIndex != "a1" || got.Provider != "codex" || got.Status != "warning" || got.ObservedAtMS != 1700000000000 {
		t.Fatalf("item = %+v", got)
	}
	if got.Plan == nil || got.Plan.PlanType != "plus" || got.Plan.PlanLabel != "Plus" {
		t.Fatalf("plan = %+v", got.Plan)
	}
	if len(got.Windows) != 1 || got.Windows[0].ID != "five_hour" || got.Windows[0].Scope != "standard" {
		t.Fatalf("windows = %+v", got.Windows)
	}
	if got.Windows[0].UsedPercent == nil || *got.Windows[0].UsedPercent != 42.5 {
		t.Fatalf("used percent = %+v", got.Windows[0].UsedPercent)
	}
	if got.Windows[0].ResetAtMS == nil || *got.Windows[0].ResetAtMS != 1700003600000 {
		t.Fatalf("reset = %+v", got.Windows[0].ResetAtMS)
	}
	if initializes.Load() != 1 {
		t.Fatalf("initialize count = %d, want 1", initializes.Load())
	}
}

// TestProtocolVersionCached: the stateless endpoint is initialized once per
// client and the negotiated version is reused, not re-negotiated every call.
func TestProtocolVersionCached(t *testing.T) {
	srv, initializes := newMCPServer(t, "2025-03-26", func([]byte) (int, string) {
		return 0, quotaListPayload()
	})
	c := New(srv.URL, "read-only-key", 5*time.Second)
	for i := 0; i < 3; i++ {
		if _, err := c.ListQuotas(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if initializes.Load() != 1 {
		t.Fatalf("initialize count = %d, want 1", initializes.Load())
	}
}

// TestContentTextFallback: a server that omits structuredContent must still be
// readable through content[0].text.
func TestContentTextFallback(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"status\":\"success\",\"data\":{\"items\":[{\"auth_index\":\"a2\",\"provider\":\"claude\",\"status\":\"healthy\",\"observed_at_ms\":1700000000000}]}}"}],"isError":false}}`
	srv, _ := newMCPServer(t, "2025-03-26", func([]byte) (int, string) { return 0, payload })
	c := New(srv.URL, "read-only-key", 5*time.Second)
	items, err := c.ListQuotas(context.Background())
	if err != nil || len(items) != 1 || items[0].AuthIndex != "a2" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}

func TestToolIsErrorRejected(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"invalid_tool_arguments"}],"isError":true}}`
	srv, _ := newMCPServer(t, "2025-03-26", func([]byte) (int, string) { return 0, payload })
	c := New(srv.URL, "read-only-key", 5*time.Second)
	if _, err := c.ListQuotas(context.Background()); err == nil {
		t.Fatal("expected an error for isError=true")
	}
}

func TestJSONRPCErrorRejected(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"invalid params"}}`
	srv, _ := newMCPServer(t, "2025-03-26", func([]byte) (int, string) { return 0, payload })
	c := New(srv.URL, "read-only-key", 5*time.Second)
	if _, err := c.ListQuotas(context.Background()); err == nil {
		t.Fatal("expected an error for a JSON-RPC error object")
	}
}

func TestEnvelopeErrorStatusRejected(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"x"}],"structuredContent":{"status":"error","code":"capability_unavailable"},"isError":true}}`
	srv, _ := newMCPServer(t, "2025-03-26", func([]byte) (int, string) { return 0, payload })
	c := New(srv.URL, "read-only-key", 5*time.Second)
	if _, err := c.ListQuotas(context.Background()); err == nil {
		t.Fatal("expected an error for a non-success envelope")
	}
}

func TestHTTPFailureRejected(t *testing.T) {
	srv, _ := newMCPServer(t, "2025-03-26", func([]byte) (int, string) {
		return http.StatusUnauthorized, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"nope"}}`
	})
	c := New(srv.URL, "read-only-key", 5*time.Second)
	_, err := c.ListQuotas(context.Background())
	if err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
	// The bearer key must never appear in an error string.
	if strings.Contains(err.Error(), "read-only-key") {
		t.Fatalf("error leaked the key: %v", err)
	}
}

func TestTransportErrorDoesNotLeakEndpoint(t *testing.T) {
	c := New("http://127.0.0.1:1/omc/api/mcp", "read-only-key", 200*time.Millisecond)
	_, err := c.ListQuotas(context.Background())
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "read-only-key") {
		t.Fatalf("error leaked endpoint/key: %v", err)
	}
}

// TestSSEResponseParsed covers a server that answers with text/event-stream
// despite the JSON preference.
func TestSSEResponseParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req rpcRequestBody
		_ = json.Unmarshal(raw, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"structuredContent\":{\"status\":\"success\",\"data\":{\"items\":[{\"auth_index\":\"sse\",\"provider\":\"codex\"}]}}}}\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "read-only-key", 5*time.Second)
	items, err := c.ListQuotas(context.Background())
	if err != nil || len(items) != 1 || items[0].AuthIndex != "sse" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}
