package main

import (
	"encoding/json"
	"testing"

	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

func runPreRequest(t *testing.T, config, messages string) string {
	t.Helper()
	p := &Plugin{}
	out, err := p.PreRequest(t.Context(), &proto.PreRequestInput{
		Ctx:          &proto.RequestContext{OrgId: "org1", ConfigJson: config},
		MessagesJson: messages,
	})
	if err != nil {
		t.Fatalf("PreRequest returned error: %v", err)
	}
	return out.GetModifiedMessagesJson()
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func decodeMessages(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var msgs []map[string]any
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatalf("decode modified messages: %v", err)
	}
	return msgs
}

// TestPreRequest_EmptySystemPrompt verifies the plugin is a passthrough when
// no system prompt is configured.
func TestPreRequest_EmptySystemPrompt(t *testing.T) {
	input := mustMarshal(t, []map[string]string{
		{"role": "user", "content": "hello"},
	})
	out := runPreRequest(t, `{}`, input)
	if out != "" {
		t.Errorf("expected passthrough (no ModifiedMessagesJson) when system_prompt is empty, got %q", out)
	}
}

// TestPreRequest_ReplaceMode_DropsExistingSystemMessages is the regression
// test for the bug where "replace" mode prepended a new system message
// without removing the existing one(s), leaving the model with two prompts.
func TestPreRequest_ReplaceMode_DropsExistingSystemMessages(t *testing.T) {
	input := mustMarshal(t, []map[string]string{
		{"role": "system", "content": "client-provided prompt"},
		{"role": "user", "content": "hello"},
	})
	out := runPreRequest(t, `{"system_prompt":"configured prompt","append":false}`, input)
	msgs := decodeMessages(t, out)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (configured system + user), got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "configured prompt" {
		t.Errorf("expected first message to be the configured system prompt, got %#v", msgs[0])
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "hello" {
		t.Errorf("expected the user message to follow, got %#v", msgs[1])
	}

	for i, m := range msgs {
		if m["role"] == "system" && m["content"] == "client-provided prompt" {
			t.Errorf("system message at index %d should have been replaced, got %#v", i, m)
		}
	}
}

// TestPreRequest_ReplaceMode_HandlesMultipleExistingSystemMessages ensures
// every existing system message is dropped, not only the first.
func TestPreRequest_ReplaceMode_HandlesMultipleExistingSystemMessages(t *testing.T) {
	input := mustMarshal(t, []map[string]string{
		{"role": "system", "content": "first client prompt"},
		{"role": "user", "content": "middle"},
		{"role": "system", "content": "second client prompt"},
	})
	out := runPreRequest(t, `{"system_prompt":"configured prompt","append":false}`, input)
	msgs := decodeMessages(t, out)

	systemCount := 0
	for _, m := range msgs {
		if m["role"] == "system" {
			systemCount++
			if m["content"] != "configured prompt" {
				t.Errorf("expected only the configured system prompt, got %#v", m)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("expected exactly 1 system message after replace, got %d: %v", systemCount, msgs)
	}
}

// TestPreRequest_ReplaceMode_NoExistingSystem covers the trivial case where
// no client system message is present: the configured one is still prepended.
func TestPreRequest_ReplaceMode_NoExistingSystem(t *testing.T) {
	input := mustMarshal(t, []map[string]string{
		{"role": "user", "content": "hello"},
	})
	out := runPreRequest(t, `{"system_prompt":"configured prompt","append":false}`, input)
	msgs := decodeMessages(t, out)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "configured prompt" {
		t.Errorf("expected configured system prompt first, got %#v", msgs[0])
	}
}

// TestPreRequest_AppendMode_Concatenates ensures append mode still merges
// with an existing system message instead of replacing it.
func TestPreRequest_AppendMode_Concatenates(t *testing.T) {
	input := mustMarshal(t, []map[string]string{
		{"role": "system", "content": "client-provided prompt"},
		{"role": "user", "content": "hello"},
	})
	out := runPreRequest(t, `{"system_prompt":"configured prompt","append":true}`, input)
	msgs := decodeMessages(t, out)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("expected system message first, got %#v", msgs[0])
	}
	content, _ := msgs[0]["content"].(string)
	if content != "client-provided prompt\n\nconfigured prompt" {
		t.Errorf("expected concatenated system prompt, got %q", content)
	}
}

// TestPreRequest_AppendMode_NoExistingSystem prepends the configured prompt
// when no client system message is there to merge with.
func TestPreRequest_AppendMode_NoExistingSystem(t *testing.T) {
	input := mustMarshal(t, []map[string]string{
		{"role": "user", "content": "hello"},
	})
	out := runPreRequest(t, `{"system_prompt":"configured prompt","append":true}`, input)
	msgs := decodeMessages(t, out)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "configured prompt" {
		t.Errorf("expected configured system prompt first, got %#v", msgs[0])
	}
}
