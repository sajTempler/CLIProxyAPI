package helps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
)

// cursorTestPNG is "\x89PNG\r\n" as base64: content is opaque to the provider.
const cursorTestPNG = "iVBORw0K"

func cursorImageBlock(mediaType, data string) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}}
}

func TestParseCursorClaudeRequestImages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + cursorTestPNG + `"}},
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}}]}]}`)
	req, err := parseCursorClaudeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	imgs := req.lastUserImageBlocks()
	if len(imgs) != 1 || imgs[0].MediaType != "image/jpeg" || string(imgs[0].Data) != "\x89PNG\r\n" {
		t.Fatalf("images = %+v", imgs)
	}
	// A document is not dropped silently: the model is told about it.
	if got := req.lastUserText(); !strings.Contains(got, "what is this?") || !strings.Contains(got, "document (PDF)") {
		t.Fatalf("text = %q", got)
	}
}

func TestParseCursorClaudeRequestRejectsUnmappableImages(t *testing.T) {
	for name, img := range map[string]string{
		"url source":  `{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}`,
		"bad type":    `{"type":"image","source":{"type":"base64","media_type":"image/tiff","data":"` + cursorTestPNG + `"}}`,
		"bad base64":  `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%%%"}}`,
		"in a result": `{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"x"}}]}`,
	} {
		_, err := parseCursorClaudeRequest([]byte(`{"messages":[{"role":"user","content":[` + img + `]}]}`))
		var reqErr *CursorRequestError
		if !errors.As(err, &reqErr) || !strings.Contains(reqErr.Msg, "image") {
			t.Errorf("%s: err = %v, want a CursorRequestError about the image", name, err)
		}
	}
}

func TestCursorStreamSendsUserImagesInline(t *testing.T) {
	rt := loadCursorFixtures(t, "plain-text")
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools}
	conv.Messages = append(conv.Messages, map[string]any{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "Reply with one short sentence: what is 2+2?"},
		cursorImageBlock("image/png", cursorTestPNG),
	}})
	streamCursor(t, newCursorTestClient(rt), "conv", "composer-2.5", conv)

	msg := rt.conn(0).sentMessages()[0].GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage()
	imgs := msg.GetSelectedContext().GetSelectedImages()
	if len(imgs) != 1 || string(imgs[0].GetData()) != "\x89PNG\r\n" || imgs[0].GetMimeType() != "image/png" || imgs[0].GetUuid() == "" {
		t.Fatalf("selected images = %v", imgs)
	}
	if msg.GetText() != "Reply with one short sentence: what is 2+2?" {
		t.Fatalf("text = %q", msg.GetText())
	}
}

func TestCursorStreamSendsToolResultImagesAsMCPContent(t *testing.T) {
	rt := loadCursorFixtures(t, "tool-roundtrip-1")
	c := newCursorTestClient(rt)
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools}
	conv.user("Use the lookup_secret tool to get the secret for key 'alpha' and tell me the value.")
	a := streamCursor(t, c, "conv", "composer-2.5", conv)
	uses := a.toolUses()
	if len(uses) != 1 {
		t.Fatalf("blocks = %v", a.Blocks)
	}
	conv.assistant(a)
	conv.Messages = append(conv.Messages, map[string]any{"role": "user", "content": []map[string]any{{
		"type": "tool_result", "tool_use_id": uses[0]["id"],
		"content": []map[string]any{{"type": "text", "text": "PURPLE-42"}, cursorImageBlock("image/webp", cursorTestPNG)},
	}}})
	streamCursor(t, c, "conv", "composer-2.5", conv)

	var content []*cursorpb.McpToolResultContentItem
	for _, m := range rt.conn(0).sentMessages() {
		if r := m.GetExecClientMessage().GetMcpResult(); r != nil {
			content = r.GetSuccess().GetContent()
		}
	}
	if len(content) != 2 || content[0].GetText().GetText() != "PURPLE-42" ||
		string(content[1].GetImage().GetData()) != "\x89PNG\r\n" || content[1].GetImage().GetMimeType() != "image/webp" {
		t.Fatalf("mcp result content = %v", content)
	}
}

// Seeding a transcript keeps the last message's images (including those in
// its tool results) and marks earlier ones.
func TestCursorStreamTranscriptKeepsLastImages(t *testing.T) {
	rt := loadCursorFixtures(t, "plain-text")
	conv := &cursorTestConversation{System: cursorTestSystem}
	conv.Messages = []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "earlier"}, cursorImageBlock("image/png", cursorTestPNG)}},
		{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "seen"}}},
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "and this one?"}, cursorImageBlock("image/gif", cursorTestPNG)}},
	}
	streamCursor(t, newCursorTestClient(rt), "conv", "composer-2.5", conv)
	msg := rt.conn(0).sentMessages()[0].GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage()
	if imgs := msg.GetSelectedContext().GetSelectedImages(); len(imgs) != 1 || imgs[0].GetMimeType() != "image/gif" {
		t.Fatalf("selected images = %v", imgs)
	}
	if !strings.Contains(msg.GetText(), "earlier\n[image]") || !strings.HasSuffix(msg.GetText(), "and this one?") {
		t.Fatalf("transcript = %q", msg.GetText())
	}
}

func TestCursorStreamRejectsURLImageBeforeAnyRun(t *testing.T) {
	rt := loadCursorFixtures(t, "plain-text")
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
	}}}})
	err := newCursorTestClient(rt).Stream(context.Background(), CursorStreamRequest{ConversationKey: "k", Model: "composer-2.5", Body: body}, func(CursorClaudeEvent) error { return nil })
	var reqErr *CursorRequestError
	if !errors.As(err, &reqErr) || rt.opened() != 0 {
		t.Fatalf("err = %v, runs opened = %d", err, rt.opened())
	}
}
