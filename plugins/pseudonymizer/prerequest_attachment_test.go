package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xolo-gateway/xolo/internal/pipeline/pipelinetest"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

// attachmentPlugin returns a plugin wired with a regex-only anonymizer, so
// PreRequest can be exercised end to end without downloading a NER model.
func attachmentPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := newPlugin()
	if _, err := p.ensureStore(cfg); err != nil {
		t.Fatalf("ensure store: %v", err)
	}
	p.mu.Lock()
	p.anons[cfg.Language] = pipelinetest.NewRegexAnonymizer()
	p.mu.Unlock()
	return p
}

// attachmentConfig pins the language so no detector — hence no model — is
// needed, and keeps the store offline and out of the user's cache directory.
func attachmentConfig(t *testing.T) Config {
	t.Helper()

	cfg := defaultConfig()
	cfg.Language = "fr"
	cfg.FallbackLanguage = "fr"
	cfg.Offline = true
	cfg.CacheDir = t.TempDir()
	cfg.Verification = false
	return cfg
}

// preRequestWithParts runs PreRequest on a single user message made of parts.
func preRequestWithParts(t *testing.T, cfg Config, parts []any) *proto.PreRequestOutput {
	t.Helper()

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	messages := []map[string]any{{"role": "user", "content": parts}}
	messagesJSON, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}

	out, err := attachmentPlugin(t, cfg).PreRequest(context.Background(), &proto.PreRequestInput{
		Ctx:          &proto.RequestContext{OrgId: "org-1", ConfigJson: string(cfgJSON)},
		MessagesJson: string(messagesJSON),
	})
	if err != nil {
		t.Fatalf("PreRequest: %v", err)
	}
	return out
}

// filePart builds an OpenAI-style file part from a testdata fixture.
func filePart(t *testing.T, fixture, mediaType string) any {
	t.Helper()

	data := readFixture(t, fixture)
	return map[string]any{
		"type": "file",
		"file": map[string]any{
			"filename":  fixture,
			"file_data": "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data),
		},
	}
}

// userMessageParts decodes the parts of the user message, skipping the system
// message the plugin may have prepended to carry its placeholder instruction.
func userMessageParts(t *testing.T, out *proto.PreRequestOutput) []any {
	t.Helper()

	var messages []map[string]any
	if err := json.Unmarshal([]byte(out.ModifiedMessagesJson), &messages); err != nil {
		t.Fatalf("unmarshal modified messages: %v", err)
	}
	for _, msg := range messages {
		if msg["role"] != "user" {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			t.Fatalf("user message content is not a parts array: %#v", msg["content"])
		}
		return parts
	}
	t.Fatalf("no user message left in the request: %s", out.ModifiedMessagesJson)
	return nil
}

func TestPreRequest_DocumentAttachmentBecomesPseudonymizedText(t *testing.T) {
	cfg := attachmentConfig(t)

	out := preRequestWithParts(t, cfg, []any{
		map[string]any{"type": "text", "text": "Résume ce document."},
		filePart(t, "sample.pdf", "application/pdf"),
	})

	if !out.Allowed {
		t.Fatalf("request rejected: %s", out.RejectionReason)
	}

	parts := userMessageParts(t, out)
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}

	doc, _ := parts[1].(map[string]any)
	if doc["type"] != "text" {
		t.Fatalf("attachment part type = %v, want text", doc["type"])
	}
	text, _ := doc["text"].(string)

	// The document content reached the model, minus its personal data.
	if !strings.Contains(text, "sample.pdf") {
		t.Errorf("attachment text does not name the file: %q", text)
	}
	if !strings.Contains(text, "Nantes") {
		t.Errorf("document content missing from the attachment text: %q", text)
	}
	if strings.Contains(text, "jean.dupont@example.com") {
		t.Errorf("email leaked to the model: %q", text)
	}
	if strings.Contains(out.ModifiedMessagesJson, "JVBER") ||
		strings.Contains(out.ModifiedMessagesJson, "file_data") {
		t.Errorf("raw file bytes still present in the request")
	}

	// The mapping is shared with the message text, so the response can be
	// restored the same way.
	var state pluginState
	if err := json.Unmarshal(out.NodeState, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if len(state.Mapping) == 0 {
		t.Errorf("no pseudonym recorded for the document entities")
	}
	if len(state.RemovedParts) != 0 {
		t.Errorf("attachment reported as removed: %+v", state.RemovedParts)
	}
}

func TestPreRequest_UnsupportedAttachmentBlocksTheRequest(t *testing.T) {
	cfg := attachmentConfig(t)
	cfg.UnsupportedAttachments = "block"

	out := preRequestWithParts(t, cfg, []any{
		map[string]any{"type": "text", "text": "Décris cette image."},
		map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG"))},
		},
	})

	if out.Allowed {
		t.Fatalf("request allowed, want it blocked")
	}
	if !strings.Contains(out.RejectionReason, reasonUnsupported) {
		t.Errorf("rejection reason = %q, want it to mention %q", out.RejectionReason, reasonUnsupported)
	}
}

func TestPreRequest_UnsupportedAttachmentRemovedWhenConfigured(t *testing.T) {
	cfg := attachmentConfig(t)
	cfg.UnsupportedAttachments = "remove"

	out := preRequestWithParts(t, cfg, []any{
		map[string]any{"type": "text", "text": "Décris cette image."},
		map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG"))},
		},
	})

	if !out.Allowed {
		t.Fatalf("request rejected: %s", out.RejectionReason)
	}
	if parts := userMessageParts(t, out); len(parts) != 1 {
		t.Errorf("len(parts) = %d, want the image dropped and the text kept", len(parts))
	}

	var state pluginState
	if err := json.Unmarshal(out.NodeState, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if len(state.RemovedParts) != 1 {
		t.Fatalf("RemovedParts = %+v, want one entry", state.RemovedParts)
	}
	if state.RemovedParts[0].Reason != reasonUnsupported {
		t.Errorf("reason = %q, want %q", state.RemovedParts[0].Reason, reasonUnsupported)
	}

	// The user is told about it in the response.
	resp, err := (&Plugin{}).PostResponse(context.Background(), &proto.PostResponseInput{
		NodeState:       out.NodeState,
		ResponseContent: "Je n'ai pas vu d'image.",
	})
	if err != nil {
		t.Fatalf("PostResponse: %v", err)
	}
	if !strings.Contains(resp.ModifiedResponseContent, reasonUnsupported) {
		t.Errorf("response does not report the removed attachment: %q", resp.ModifiedResponseContent)
	}
}

func TestPreRequest_ScannedDocumentIsNotSilentlyAccepted(t *testing.T) {
	cfg := attachmentConfig(t)

	// A PDF the plugin can open but draws no text from would otherwise reach
	// the model unexamined.
	out := preRequestWithParts(t, cfg, []any{filePart(t, "empty.pdf", "application/pdf")})

	if out.Allowed {
		t.Fatalf("request allowed, want it blocked")
	}
	if !strings.Contains(out.RejectionReason, reasonNoText) {
		t.Errorf("rejection reason = %q, want it to mention %q", out.RejectionReason, reasonNoText)
	}
}

func TestPreRequest_AttachmentDisabledFallsBackToThePolicy(t *testing.T) {
	cfg := attachmentConfig(t)
	cfg.ProcessAttachments = false
	cfg.UnsupportedAttachments = "remove"

	out := preRequestWithParts(t, cfg, []any{
		map[string]any{"type": "text", "text": "Résume ce document."},
		filePart(t, "sample.csv", "text/csv"),
	})

	if !out.Allowed {
		t.Fatalf("request rejected: %s", out.RejectionReason)
	}
	if parts := userMessageParts(t, out); len(parts) != 1 {
		t.Errorf("len(parts) = %d, want the document dropped", len(parts))
	}
}

// A part type the plugin does not recognize used to fall to the same default
// as an attachment — read as a file, found to carry no inline bytes, and
// refused or stripped. It carries no more inline bytes now than before, but
// isAttachmentPart no longer claims it, so it is forwarded untouched instead
// of being treated as an unreadable file.
// THE REGRESSION BORNHOLM'S SECOND REVIEW ON #32 CAUGHT. input_audio genuinely
// carries a human-supplied file (base64 in input_audio.data) — narrowing
// default: to isAttachmentPart must not let it slip into the "unknown type,
// forward untouched" bucket alongside tool and thinking blocks, or an
// operator who set unsupported_attachments: block loses that protection
// without a word.
func TestPreRequest_InputAudioFollowsTheAttachmentPolicy(t *testing.T) {
	cfg := attachmentConfig(t)
	cfg.UnsupportedAttachments = "block"

	out := preRequestWithParts(t, cfg, []any{
		map[string]any{
			"type": "input_audio",
			"input_audio": map[string]any{
				"data":   "UklGRiQAAABXQVZFZm10IBAAAAABAAEA",
				"format": "wav",
			},
		},
	})

	if out.Allowed {
		t.Fatalf("input_audio carries a real file and must follow unsupported_attachments, got Allowed=true")
	}
}

// Same shape of bug for container_upload: a file already uploaded to the code
// execution container by file_id, referenced but never inlined — a real
// attachment, not a tool or thinking block.
func TestPreRequest_ContainerUploadFollowsTheAttachmentPolicy(t *testing.T) {
	cfg := attachmentConfig(t)
	cfg.UnsupportedAttachments = "block"

	out := preRequestWithParts(t, cfg, []any{
		map[string]any{
			"type":    "container_upload",
			"file_id": "file_abc123",
		},
	})

	if out.Allowed {
		t.Fatalf("container_upload references a real file and must follow unsupported_attachments, got Allowed=true")
	}
}

func TestPreRequest_UnrecognizedPartTypeIsLeftUntouched(t *testing.T) {
	cfg := attachmentConfig(t)
	cfg.UnsupportedAttachments = "block"

	out := preRequestWithParts(t, cfg, []any{
		map[string]any{
			"type":  "some_future_provider_block",
			"value": "peu importe",
		},
	})

	if !out.Allowed {
		t.Fatalf("an unrecognized part type must never refuse the request, got: %s", out.RejectionReason)
	}
	parts := userMessageParts(t, out)
	if len(parts) != 1 {
		t.Fatalf("expected the part kept as is, got %#v", parts)
	}
	part, ok := parts[0].(map[string]any)
	if !ok || part["type"] != "some_future_provider_block" || part["value"] != "peu importe" {
		t.Errorf("part was altered instead of forwarded as is: %#v", parts[0])
	}
}

func TestIsAttachmentPart(t *testing.T) {
	for _, partType := range []string{"file", "input_file", "input_image", "input_audio", "image_url", "document", "image", "container_upload"} {
		if !isAttachmentPart(partType) {
			t.Errorf("isAttachmentPart(%q) = false, want true", partType)
		}
	}
	for _, partType := range []string{"text", "tool_use", "tool_result", "thinking", "redacted_thinking", "some_future_provider_block", ""} {
		if isAttachmentPart(partType) {
			t.Errorf("isAttachmentPart(%q) = true, want false", partType)
		}
	}
}

func TestPreRequest_AttachmentSharesPseudonymsWithTheMessage(t *testing.T) {
	cfg := attachmentConfig(t)

	// The same email in the message and in the document must yield the same
	// placeholder, otherwise deanonymization would restore only one of them.
	out := preRequestWithParts(t, cfg, []any{
		map[string]any{"type": "text", "text": "Le contact est jean.dupont@example.com."},
		filePart(t, "sample.csv", "text/csv"),
	})

	if !out.Allowed {
		t.Fatalf("request rejected: %s", out.RejectionReason)
	}

	var state pluginState
	if err := json.Unmarshal(out.NodeState, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	occurrences := 0
	for _, original := range state.Mapping {
		if original == "jean.dupont@example.com" {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Errorf("email mapped to %d placeholders, want a single shared one: %v", occurrences, state.Mapping)
	}
}
