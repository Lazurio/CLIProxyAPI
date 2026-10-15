package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const claudeThreadCreateFixture = `{"model":"claude-opus-5-5","thread":{"type":"create"},"system":"Test assistant","messages":[{"role":"user","content":"Remember the word orchard."}],"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}],"thinking":{"type":"adaptive"},"max_tokens":128}`
const claudeThreadContinueFixture = `{"model":"claude-opus-5-5","thread":{"type":"continue","previous_message_id":"msg-thread-1"},"messages":[{"role":"user","content":"What word did I ask you to remember?"}],"thinking":{"type":"adaptive"},"max_tokens":128}`

// Claude Code's Agent SDK omits tools on delta requests. Re-deriving betas from
// that partial body drops advanced-tool-use and makes Anthropic reject the turn.
func TestClaudeExecutor_ThreadContinuationKeepsCreationBetas(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var createdBetas string
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				calls++
				if gjson.GetBytes(body, "thread.type").String() == "create" {
					createdBetas = r.Header.Get("Anthropic-Beta")
					if !strings.Contains(createdBetas, claudeAdvancedToolUseBeta) {
						t.Error("creation must exercise body-derived advanced-tool-use beta")
					}
				} else if r.Header.Get("Anthropic-Beta") != createdBetas {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"thread: differing field(s): anthropic-beta"}}`)
					return
				}
				writeClaudeThreadTestResponse(w, stream, calls)
			}))
			defer server.Close()
			executor := NewClaudeExecutor(&config.Config{})
			auth := claudeThreadTestAuth("account-a", server.URL)
			for _, payload := range []string{claudeThreadCreateFixture, claudeThreadContinueFixture} {
				// Production creates scoped executor copies; their thread state must be shared.
				scoped := executor.ForAPIKey().(*ClaudeExecutor)
				if err := executeClaudeThreadTest(scoped, auth, payload, stream); err != nil {
					t.Fatalf("thread turn failed: %v", err)
				}
			}
			if calls != 2 {
				t.Fatalf("upstream calls = %d, want exactly 2 without retries", calls)
			}
		})
	}
}

func TestClaudeExecutor_ThreadAccountChangeRequestsReplay(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeClaudeThreadTestResponse(w, false, calls)
	}))
	defer server.Close()
	executor := NewClaudeExecutor(&config.Config{})
	auth := claudeThreadTestAuth("account-a", server.URL)
	if err := executeClaudeThreadTest(executor, auth, claudeThreadCreateFixture, false); err != nil {
		t.Fatal(err)
	}
	// A refreshed token for the same auth record must preserve the thread.
	auth.Attributes["api_key"] = "sk-ant-oat-refreshed-token"
	if err := executeClaudeThreadTest(executor, auth, claudeThreadContinueFixture, false); err != nil {
		t.Fatalf("token refresh broke thread: %v", err)
	}
	before := calls
	other := claudeThreadTestAuth("account-b", server.URL)
	err := executeClaudeThreadTest(executor, other, claudeThreadContinueFixture, false)
	if err == nil {
		t.Fatal("account change must request full replay before sending a delta upstream")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusNotFound {
		t.Fatalf("error = %v, want recoverable HTTP 404", err)
	}
	scoped, ok := err.(cliproxyexecutor.RequestScopedError)
	if !ok || !scoped.IsRequestScoped() {
		t.Fatalf("replay signal must not mark a healthy account unavailable: %v", err)
	}
	if calls != before {
		t.Fatal("delta was sent to a different account")
	}
	if err := executeClaudeThreadTest(executor, other, claudeThreadCreateFixture, false); err != nil {
		t.Fatalf("full replay on replacement account failed: %v", err)
	}
}

func claudeThreadTestAuth(id, baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: id, Provider: "claude",
		Attributes: map[string]string{"api_key": "sk-ant-oat-test-" + id, "base_url": baseURL},
		Metadata:   claudeOAuthTestMetadata(),
	}
}

func executeClaudeThreadTest(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload string, stream bool, extraBeta ...string) error {
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.301 (external, sdk-ts, agent-sdk/0.3.276)")
	headers.Set("X-App", "cli")
	headers.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,message-threads-2026-08-12")
	if len(extraBeta) > 0 {
		headers.Set("Anthropic-Beta", headers.Get("Anthropic-Beta")+","+extraBeta[0])
	}
	req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(payload)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: headers}
	if !stream {
		_, err := executor.Execute(context.Background(), auth, req, opts)
		return err
	}
	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		return err
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return chunk.Err
		}
	}
	return nil
}

func TestClaudeExecutor_ThreadFingerprintChangesRequireReplay(t *testing.T) {
	for _, change := range []string{"missing state", "caller betas", "operator betas", "upstream"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				writeClaudeThreadTestResponse(w, false, calls)
			}))
			defer server.Close()
			cfg := &config.Config{}
			executor := NewClaudeExecutor(cfg)
			auth := claudeThreadTestAuth("account-a", server.URL)
			if change != "missing state" {
				if err := executeClaudeThreadTest(executor, auth, claudeThreadCreateFixture, false); err != nil {
					t.Fatal(err)
				}
			}
			before := calls
			var extraBeta []string
			switch change {
			case "caller betas":
				extraBeta = []string{"changed-feature-2099-01-01"}
			case "operator betas":
				cfg.ClaudeHeaderDefaults.AnthropicBetaStrip = []string{claudeAdvancedToolUseBeta}
			case "upstream":
				auth.Attributes["base_url"] = server.URL + "/other"
			}
			err := executeClaudeThreadTest(executor, auth, claudeThreadContinueFixture, false, extraBeta...)
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusNotFound || calls != before {
				t.Fatalf("want local replay signal; error=%v, upstream calls=%d (before %d)", err, calls, before)
			}
		})
	}
}

// Exercise the actual manager + sticky selector + Claude executor with four
// credentials. Once the first account reaches a 5h limit, the client gets one
// replay signal, then creates its full conversation on a healthy account and
// stays there. No delta is submitted under a different account's thread ID.
func TestClaudeExecutor_ThreadStickyQuotaFailover(t *testing.T) {
	var owner string
	exhausted := false
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Header.Get("Authorization")
		calls++
		if owner == "" {
			owner = key
		}
		if exhausted && key == owner {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"5h usage exhausted"}}`)
			return
		}
		if gjson.GetBytes(body, "thread.type").String() == "continue" && key != owner {
			t.Error("cross-account delta reached upstream")
		}
		writeClaudeThreadTestResponse(w, false, calls)
	}))
	defer server.Close()
	selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{TTL: 24 * time.Hour, Fallback: &cliproxyauth.RoundRobinSelector{}})
	defer selector.Stop()
	manager := cliproxyauth.NewManager(nil, selector, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
	reg := registry.GetGlobalRegistry()
	for i := 0; i < 4; i++ {
		auth := claudeThreadTestAuth(uuid.NewString(), server.URL)
		reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"X-Claude-Code-Session-Id": {uuid.NewString()}}}
	run := func(payload string) (cliproxyexecutor.Response, error) {
		return manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(payload)}, opts)
	}
	if _, err := run(claudeThreadCreateFixture); err != nil {
		t.Fatal(err)
	}
	if _, err := run(claudeThreadContinueFixture); err != nil {
		t.Fatalf("sticky continuation failed: %v", err)
	}
	exhausted = true
	_, err := run(claudeThreadContinueFixture)
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusNotFound {
		t.Fatalf("quota failover should request client replay, got %v", err)
	}
	response, err := run(claudeThreadCreateFixture)
	if err != nil {
		t.Fatalf("replay on replacement failed: %v", err)
	}
	// The replacement is now the legitimate thread owner.
	previous := gjson.GetBytes(response.Payload, "id").String()
	owner = ""
	exhausted = false
	continuation := strings.ReplaceAll(claudeThreadContinueFixture, "msg-thread-1", previous)
	if _, err := run(continuation); err != nil {
		t.Fatalf("replacement continuation failed: %v", err)
	}
	if calls != 5 {
		t.Fatalf("upstream calls=%d, want create + continue + one 429 + replay + continue", calls)
	}
}

func writeClaudeThreadTestResponse(w http.ResponseWriter, stream bool, id int) {
	message := fmt.Sprintf(`{"id":"msg-thread-%d","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"orchard"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, id)
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":%s}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", message)
}
