package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// The file watcher rebinds executors when any account is added or refreshed.
// Existing threads must still continue with their aliases and original betas,
// while the next request uses the updated token from the auth manager.
func TestClaudeExecutorBinding_AuthUpdatesPreserveThreads(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var mu sync.Mutex
			var tokens []string
			var creationBetas string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				tokens = append(tokens, r.Header.Get("Authorization"))
				if gjson.GetBytes(body, "thread.type").String() == "create" {
					creationBetas = r.Header.Get("Anthropic-Beta")
				} else if r.Header.Get("Anthropic-Beta") != creationBetas {
					t.Error("auth update changed the thread's upstream beta header")
				}
				message := fmt.Sprintf(`{"id":"msg-refresh-%d","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"orchard"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, len(tokens))
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, message)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":%s}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", message)
			}))
			defer server.Close()

			service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
			ctx := context.Background()
			account := &coreauth.Auth{
				ID: "claude-refresh-a", Provider: "claude", Status: coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "sk-ant-oat-before-refresh", "base_url": server.URL},
				Metadata: map[string]any{
					"account_uuid":                        "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
					claudeauth.ClaudeDeviceIDsMetadataKey: []string{"0000000000000000000000000000000000000000000000000000000000000000"},
				},
			}
			other := account.Clone()
			other.ID = "claude-refresh-b"
			t.Cleanup(func() {
				GlobalModelRegistry().UnregisterClient(account.ID)
				GlobalModelRegistry().UnregisterClient(other.ID)
			})
			update := func(auth *coreauth.Auth, action watcher.AuthUpdateAction) {
				service.handleAuthUpdate(ctx, watcher.AuthUpdate{Action: action, ID: auth.ID, Auth: auth})
			}
			update(account, watcher.AuthUpdateActionAdd)
			call := func(payload string) {
				t.Helper()
				exec, ok := service.coreManager.Executor("claude")
				if !ok {
					t.Fatal("Claude executor is missing")
				}
				auth, ok := service.coreManager.GetByID(account.ID)
				if !ok {
					t.Fatal("Claude account is missing")
				}
				headers := http.Header{}
				headers.Set("User-Agent", "claude-cli/2.1.301 (external, sdk-ts, agent-sdk/0.3.276)")
				headers.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,message-threads-2026-08-12")
				req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(payload)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: headers}
				if !stream {
					if _, errExecute := exec.Execute(ctx, auth, req, opts); errExecute != nil {
						t.Fatalf("thread request after auth update failed: %v", errExecute)
					}
					return
				}
				result, errStream := exec.ExecuteStream(ctx, auth, req, opts)
				if errStream != nil {
					t.Fatalf("thread stream after auth update failed: %v", errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("thread stream chunk failed: %v", chunk.Err)
					}
				}
			}
			call(`{"model":"claude-opus-5-5","thread":{"type":"create"},"messages":[{"role":"user","content":"Remember orchard."}],"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}],"max_tokens":128}`)
			continuation := func(previous int) string {
				return fmt.Sprintf(`{"model":"claude-opus-5-5","thread":{"type":"continue","previous_message_id":"msg-refresh-%d"},"messages":[{"role":"user","content":"Which word?"}],"max_tokens":128}`, previous)
			}
			update(other, watcher.AuthUpdateActionAdd)
			call(continuation(1))
			refreshed, _ := service.coreManager.GetByID(account.ID)
			refreshed.Attributes["api_key"] = "sk-ant-oat-after-refresh"
			update(refreshed, watcher.AuthUpdateActionModify)
			call(continuation(2))
			other.Label = "unrelated account changed"
			update(other, watcher.AuthUpdateActionModify)
			call(continuation(3))
			mu.Lock()
			defer mu.Unlock()
			if len(tokens) != 4 {
				t.Fatalf("upstream calls = %d, want 4 without replay", len(tokens))
			}
			for i, token := range tokens {
				want := "Bearer sk-ant-oat-before-refresh"
				if i >= 2 {
					want = "Bearer sk-ant-oat-after-refresh"
				}
				if token != want {
					t.Errorf("request %d did not use the current account token", i)
				}
			}
		})
	}
}

func TestClaudeExecutorBinding_ConfigReplacement(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
			auth := &coreauth.Auth{ID: "claude-config", Provider: "claude", Status: coreauth.StatusActive}
			service.ensureExecutorsForAuth(auth)
			before, _ := service.coreManager.Executor("claude")
			if !force {
				service.cfg = &config.Config{}
			}
			service.ensureExecutorsForAuthWithMode(auth, force)
			after, _ := service.coreManager.Executor("claude")
			if before == after || after == nil {
				t.Fatal("explicit replacement or a new config must bind a fresh executor")
			}
		})
	}
}
