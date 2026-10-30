package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// A Codex client signed in with ChatGPT can use this proxy as its gateway
// (OpenAI's "ChatGPT sign-in + gateway" pattern): its provider sets
// requires_openai_auth = true and sends the proxy key in its own header. Codex
// then also sends the person's own ChatGPT token and account. The proxy admits
// the caller by the proxy key only and serves the request from its own
// account: upstream sees that account's credential and never the person's
// token, account or the proxy key, while the requested service tier (Codex
// Fast mode) passes through. Lazurio's AI pool relies on this contract
// (Lazurio/LazurioPlatform#735).
const (
	chatGPTGatewayProxyKey      = "proxy-client-key"
	chatGPTGatewayPersonToken   = "person-chatgpt-access-token"
	chatGPTGatewayPersonAccount = "person-chatgpt-account"
	chatGPTGatewayServingToken  = "serving-account-token"
	chatGPTGatewayServingAcct   = "serving-account"
	chatGPTGatewayModel         = "gpt-6.1-sol"
)

type chatGPTGatewayUpstream struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func (u *chatGPTGatewayUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.requests = append(u.requests, r.Clone(r.Context()))
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_gateway","object":"response","status":"completed","model":"gpt-6.1-sol","service_tier":"default","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
}

// newChatGPTGatewayServer serves Codex from one OAuth account whose upstream
// is a local test server, so the test never reaches the ChatGPT backend.
func newChatGPTGatewayServer(t *testing.T) (*Server, *chatGPTGatewayUpstream) {
	t.Helper()
	upstream := &chatGPTGatewayUpstream{}
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)

	server := newTestServerWithConfig(t, &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{chatGPTGatewayProxyKey}}})
	server.handlers.AuthManager.RegisterExecutor(executor.NewCodexAutoExecutor(server.getConfig()))
	credential := &auth.Auth{
		ID:         "codex-chatgpt-gateway-serving",
		Provider:   "codex",
		Status:     auth.StatusActive,
		Attributes: map[string]string{"base_url": backend.URL},
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": chatGPTGatewayServingToken,
			"account_id":   chatGPTGatewayServingAcct,
		},
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, "codex", []*registry.ModelInfo{{ID: chatGPTGatewayModel, Object: "model", OwnedBy: "openai", Type: "openai"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(credential.ID)
	})
	if _, errRegister := server.handlers.AuthManager.Register(t.Context(), credential); errRegister != nil {
		t.Fatalf("register Codex credential: %v", errRegister)
	}
	return server, upstream
}

// setChatGPTGatewayHeaders sets the headers codex-cli 0.165.0 sends to a
// Responses provider with requires_openai_auth = true and
// http_headers = { X-Api-Key = ... } while signed in with ChatGPT, with
// synthetic values.
func setChatGPTGatewayHeaders(req *http.Request, proxyKey bool) {
	req.Header.Set("Authorization", "Bearer "+chatGPTGatewayPersonToken)
	req.Header.Set("ChatGPT-Account-ID", chatGPTGatewayPersonAccount)
	if proxyKey {
		req.Header.Set("X-Api-Key", chatGPTGatewayProxyKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Originator", "codex_exec")
	req.Header.Set("User-Agent", "codex_exec/0.165.0 (Ubuntu 24.4.0; x86_64) unknown (codex_exec; 0.165.0)")
	req.Header.Set("Session-Id", "019a0000-0000-7000-8000-000000000001")
	req.Header.Set("Thread-Id", "019a0000-0000-7000-8000-000000000001")
	req.Header.Set("X-Client-Request-Id", "019a0000-0000-7000-8000-000000000001")
	req.Header.Set("X-Codex-Window-Id", "019a0000-0000-7000-8000-000000000001:0")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	req.Header.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
}

func TestCodexChatGPTSignInGatewayServesFromItsOwnAccount(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		tier     string
		wantHint string
	}{
		{name: "fast", tier: "priority", wantHint: "model=gpt-6.1-sol;tier=priority"},
		{name: "standard", wantHint: "model=gpt-6.1-sol"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server, upstream := newChatGPTGatewayServer(t)

			body := `{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"reasoning":{"effort":"low"},"store":false,"stream":true}`
			if testCase.tier != "" {
				body = strings.Replace(body, `"store":false`, `"service_tier":"`+testCase.tier+`","store":false`, 1)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			setChatGPTGatewayHeaders(req, true)
			recorder := httptest.NewRecorder()
			server.engine.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			if len(upstream.requests) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(upstream.requests))
			}
			sent, sentBody := upstream.requests[0], upstream.bodies[0]
			if got := sent.URL.Path; got != "/responses" {
				t.Errorf("upstream path = %q, want /responses", got)
			}
			if got := sent.Header.Get("Authorization"); got != "Bearer "+chatGPTGatewayServingToken {
				t.Errorf("upstream Authorization = %q, want the serving account's token", got)
			}
			if got := sent.Header.Get("Chatgpt-Account-Id"); got != chatGPTGatewayServingAcct {
				t.Errorf("upstream Chatgpt-Account-Id = %q, want the serving account", got)
			}
			if got := sent.Header.Get("X-Codex-Routing-Hint"); got != testCase.wantHint {
				t.Errorf("upstream X-Codex-Routing-Hint = %q, want %q", got, testCase.wantHint)
			}
			for name, values := range sent.Header {
				for _, value := range values {
					for _, secret := range []string{chatGPTGatewayPersonToken, chatGPTGatewayPersonAccount, chatGPTGatewayProxyKey} {
						if strings.Contains(value, secret) {
							t.Errorf("upstream header %s carries the caller's %q", name, secret)
						}
					}
				}
			}
			for _, secret := range []string{chatGPTGatewayPersonToken, chatGPTGatewayPersonAccount, chatGPTGatewayProxyKey} {
				if strings.Contains(string(sentBody), secret) {
					t.Errorf("upstream body carries the caller's %q", secret)
				}
			}
			tier := gjson.GetBytes(sentBody, "service_tier")
			if testCase.tier == "" {
				if tier.Exists() {
					t.Errorf("upstream service_tier = %s, want absent", tier.Raw)
				}
			} else if tier.String() != testCase.tier {
				t.Errorf("upstream service_tier = %q, want %q", tier.String(), testCase.tier)
			}
		})
	}
}

func TestCodexChatGPTSignInGatewayRefusesThePersonsTokenAlone(t *testing.T) {
	server, upstream := newChatGPTGatewayServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","input":"hi","service_tier":"priority","stream":true}`))
	setChatGPTGatewayHeaders(req, false)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream requests = %d, want none", len(upstream.requests))
	}
}

func TestCodexChatGPTSignInGatewayCatalogOffersFast(t *testing.T) {
	server, _ := newChatGPTGatewayServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.165.0", nil)
	setChatGPTGatewayHeaders(req, true)
	req.Header.Del("Content-Type")
	req.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var catalog struct {
		Models []struct {
			Slug         string `json:"slug"`
			ServiceTiers []struct {
				ID string `json:"id"`
			} `json:"service_tiers"`
		} `json:"models"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &catalog); errDecode != nil {
		t.Fatalf("decode Codex catalog: %v; body=%s", errDecode, recorder.Body.String())
	}
	for _, model := range catalog.Models {
		if model.Slug != chatGPTGatewayModel {
			continue
		}
		for _, tier := range model.ServiceTiers {
			if tier.ID == "priority" {
				return
			}
		}
		t.Fatalf("%s service_tiers = %+v, want the priority (Fast) tier", chatGPTGatewayModel, model.ServiceTiers)
	}
	t.Fatalf("Codex catalog has no %s entry", chatGPTGatewayModel)
}
