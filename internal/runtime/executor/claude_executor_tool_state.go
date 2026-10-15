package executor

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const claudeOAuthToolAliasStateLimit = 1024

// claudeOAuthToolAliasStoreInitMu guards lazy allocation for executors built
// without a constructor. It lives outside ClaudeExecutor because ForAPIKey
// copies the executor by value, and a mutex field would be copied with it.
var claudeOAuthToolAliasStoreInitMu sync.Mutex

type claudeOAuthToolAliasStore struct {
	mu      sync.Mutex
	entries map[string]claudeOAuthThreadState
	order   []string
}

// Thread betas are immutable upstream, even when a continuation omits the
// tools/thinking/cache fields from which CPA originally derived those betas.
// Keep their wire value alongside the existing bounded, per-message alias state.
type claudeThreadFingerprint struct {
	anthropicBeta string
	callerBetas   string
	operatorBetas string
	credential    string
	upstream      string
}

type claudeOAuthThreadState struct {
	aliases     map[string]string
	fingerprint *claudeThreadFingerprint
}

func (s *claudeOAuthToolAliasStore) load(keys []string) (claudeOAuthThreadState, bool) {
	if s == nil {
		return claudeOAuthThreadState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		state, ok := s.entries[key]
		if !ok {
			continue
		}
		state.aliases = cloneClaudeOAuthToolAliasMap(state.aliases)
		return state, true
	}
	return claudeOAuthThreadState{}, false
}

func (s *claudeOAuthToolAliasStore) save(keys []string, aliases map[string]string, fingerprint *claudeThreadFingerprint) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]claudeOAuthThreadState)
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, exists := s.entries[key]; !exists {
			s.order = append(s.order, key)
		}
		s.entries[key] = claudeOAuthThreadState{aliases: cloneClaudeOAuthToolAliasMap(aliases), fingerprint: fingerprint}
	}
	for len(s.order) > claudeOAuthToolAliasStateLimit {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

func cloneClaudeOAuthToolAliasMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return map[string]string{}
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (e *ClaudeExecutor) claudeOAuthToolAliasStore() *claudeOAuthToolAliasStore {
	claudeOAuthToolAliasStoreInitMu.Lock()
	defer claudeOAuthToolAliasStoreInitMu.Unlock()
	if e.oauthToolAliases == nil {
		e.oauthToolAliases = &claudeOAuthToolAliasStore{}
	}
	return e.oauthToolAliases
}

func claudeOAuthToolAliasKeys(payload []byte, messageID string) []string {
	keys := make([]string, 0, 2)
	if previousMessageID := gjson.GetBytes(payload, "thread.previous_message_id").String(); previousMessageID != "" {
		keys = append(keys, "message:"+previousMessageID)
	}
	if messageID != "" {
		keys = append(keys, "message:"+messageID)
	}
	return keys
}

func claudeThreadContinuationNeedsAliasState(payload []byte) bool {
	if gjson.GetBytes(payload, "thread.type").String() != "continue" {
		return false
	}
	previousMessageID := gjson.GetBytes(payload, "thread.previous_message_id")
	if !previousMessageID.Exists() || previousMessageID.String() == "" {
		return false
	}
	tools := gjson.GetBytes(payload, "tools")
	return !tools.Exists() || !tools.IsArray() || len(tools.Array()) == 0
}

func (e *ClaudeExecutor) prepareClaudeOAuthToolNamesForRequest(payload []byte, options claudeMCPAliasOptions) ([]byte, map[string]string, error) {
	if claudeThreadContinuationNeedsAliasState(payload) {
		state, ok := e.claudeOAuthToolAliasStore().load(claudeOAuthToolAliasKeys(payload, ""))
		if !ok {
			return nil, nil, newClaudeThreadNotFoundError()
		}
		return payload, state.aliases, nil
	}
	remapped, aliases := prepareClaudeOAuthToolNamesForUpstream(payload, options)
	return remapped, aliases, nil
}

func (e *ClaudeExecutor) rememberClaudeOAuthToolAliases(payload []byte, aliases map[string]string, messageID string, fingerprints ...claudeThreadFingerprint) {
	if gjson.GetBytes(payload, "thread.type").String() == "" {
		return
	}
	keys := claudeOAuthToolAliasKeys(payload, messageID)
	if len(keys) == 0 {
		return
	}
	var fingerprint *claudeThreadFingerprint
	if len(fingerprints) > 0 {
		value := fingerprints[0]
		fingerprint = &value
	}
	e.claudeOAuthToolAliasStore().save(keys, aliases, fingerprint)
}

// Run after final header construction. Do not change the business payload: a
// delta cannot be turned into a new thread without the client's full history.
// A missing state, account switch or deliberate caller beta change instead uses
// the existing request-scoped 404 replay signal understood by Claude Code.
func (e *ClaudeExecutor) prepareClaudeThreadFingerprint(r *http.Request, payload []byte, auth *cliproxyauth.Auth, apiKey string, incoming http.Header) (claudeThreadFingerprint, error) {
	fingerprint := claudeThreadFingerprint{
		anthropicBeta: r.Header.Get("Anthropic-Beta"),
		callerBetas:   strings.TrimSpace(strings.Join(helps.HeaderValuesCaseInsensitive(incoming, "Anthropic-Beta"), ",")),
		upstream:      r.URL.String(),
	}
	if e.cfg != nil {
		hd := e.cfg.ClaudeHeaderDefaults
		fingerprint.operatorBetas = fmt.Sprintf("%q|%q", hd.AnthropicBetaStrip, hd.AnthropicBetaAppend)
	}
	if auth != nil {
		fingerprint.credential = strings.TrimSpace(auth.ID)
		if fingerprint.credential == "" {
			fingerprint.credential = strings.TrimSpace(auth.FileName)
		}
	}
	if fingerprint.credential == "" {
		// Anonymous embedders have no stable auth-record ID. Keep secrets out of
		// the cache and require a replay when their credential changes.
		fingerprint.credential = fmt.Sprintf("key:%x", sha256.Sum256([]byte(apiKey)))
	}
	if gjson.GetBytes(payload, "thread.type").String() != "continue" {
		return fingerprint, nil
	}
	state, ok := e.claudeOAuthToolAliasStore().load(claudeOAuthToolAliasKeys(payload, ""))
	if !ok || state.fingerprint == nil ||
		state.fingerprint.credential != fingerprint.credential ||
		state.fingerprint.upstream != fingerprint.upstream ||
		state.fingerprint.callerBetas != fingerprint.callerBetas ||
		state.fingerprint.operatorBetas != fingerprint.operatorBetas {
		return fingerprint, newClaudeThreadNotFoundError()
	}
	fingerprint.anthropicBeta = state.fingerprint.anthropicBeta
	if fingerprint.anthropicBeta == "" {
		r.Header.Del("Anthropic-Beta")
	} else {
		r.Header.Set("Anthropic-Beta", fingerprint.anthropicBeta)
	}
	return fingerprint, nil
}

const claudeThreadNotFoundErrorMessage = "No thread state was found for the requested previous_message_id. Replay the full conversation with thread create to start a new Thread."

type claudeThreadNotFoundError struct {
	statusErr
}

func newClaudeThreadNotFoundError() claudeThreadNotFoundError {
	return claudeThreadNotFoundError{statusErr: statusErr{
		code: http.StatusNotFound,
		msg:  claudeThreadNotFoundErrorMessage,
	}}
}

func (claudeThreadNotFoundError) IsRequestScoped() bool { return true }

func (claudeThreadNotFoundError) ResponseBody() []byte {
	return []byte(`{"type":"error","error":{"type":"not_found_error","message":"` + claudeThreadNotFoundErrorMessage + `"}}`)
}

var _ cliproxyexecutor.RequestScopedError = claudeThreadNotFoundError{}
