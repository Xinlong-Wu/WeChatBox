package monitor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lingobridge/internal/config"
	"lingobridge/internal/core"
	"lingobridge/internal/llm"
	"lingobridge/internal/platform/wechat/api"
	"lingobridge/internal/platform/wechat/cdn"
	"lingobridge/internal/store"
)

type fakeWechatClient struct {
	sent       []*api.WeixinMessage
	typing     []int
	stops      int
	sendErr    error
	failSendAt int
	sendCalls  int
	typingErr  error
	typingCh   chan int
	uploadResp *api.GetUploadUrlResp
	uploadErr  error
	uploadReq  *api.GetUploadUrlReq
}

func (f *fakeWechatClient) GetUpdatesContext(ctx context.Context, buf string) (*api.GetUpdatesResp, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return &api.GetUpdatesResp{}, nil
}

func (f *fakeWechatClient) SendMessage(msg *api.WeixinMessage) error {
	f.sendCalls++
	if f.failSendAt > 0 && f.sendCalls == f.failSendAt {
		if f.sendErr != nil {
			return f.sendErr
		}
		return errors.New("send failed")
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeWechatClient) GetUploadUrl(req *api.GetUploadUrlReq) (*api.GetUploadUrlResp, error) {
	f.uploadReq = req
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	if f.uploadResp != nil {
		return f.uploadResp, nil
	}
	return &api.GetUploadUrlResp{UploadFullURL: "https://cdn.test/upload"}, nil
}

func (f *fakeWechatClient) GetConfig(ilinkUserID, contextToken string) (*api.GetConfigResp, error) {
	return &api.GetConfigResp{TypingTicket: "ticket"}, nil
}

func (f *fakeWechatClient) SendTyping(ilinkUserID, typingTicket string, status int) error {
	f.typing = append(f.typing, status)
	if f.typingCh != nil {
		select {
		case f.typingCh <- status:
		default:
		}
	}
	if f.typingErr != nil {
		return f.typingErr
	}
	return nil
}

func (f *fakeWechatClient) NotifyStart() error { return nil }
func (f *fakeWechatClient) NotifyStop() error {
	f.stops++
	return nil
}

type fakeCursorStore struct{}

func (f *fakeCursorStore) GetSyncBuf(accountID string) (string, error) {
	return "", nil
}

func (f *fakeCursorStore) SaveSyncBuf(accountID, buf string) error {
	return nil
}

type captureHandler struct {
	called bool
	msg    core.InboundMessage
}

func (h *captureHandler) Handle(ctx context.Context, msg core.InboundMessage, sender core.Sender) error {
	h.called = true
	h.msg = msg
	return nil
}

type fakeConversationManager struct {
	sess           *store.Session
	conv           *store.Conversation
	saved          *store.Conversation
	sessions       []store.Session
	modelBySession map[string]string
	models         []string
}

func (f *fakeConversationManager) GetOrCreateCurrentSession(userID string) (*store.Session, error) {
	if f.sess != nil {
		return f.sess, nil
	}
	return &store.Session{ID: "session", UserID: userID, Name: "default", Current: true}, nil
}

func (f *fakeConversationManager) GetSession(userID, sessionID string) (*store.Session, error) {
	if f.sess != nil && f.sess.ID == sessionID && f.sess.UserID == userID && !f.sess.Archived {
		return f.sess, nil
	}
	return &store.Session{ID: sessionID, UserID: userID, Name: "target"}, nil
}

func (f *fakeConversationManager) CurrentSession(userID string) (*store.Session, error) {
	return f.GetOrCreateCurrentSession(userID)
}

func (f *fakeConversationManager) LoadHistory(userID, sessionID string) (*store.Conversation, error) {
	if f.conv != nil {
		return f.conv, nil
	}
	return &store.Conversation{}, nil
}

func (f *fakeConversationManager) SaveHistoryCAS(userID, sessionID string, expectedRevision int64, conv *store.Conversation) (int64, error) {
	conv.Revision = expectedRevision + 1
	f.saved = conv
	return conv.Revision, nil
}

func (f *fakeConversationManager) CreateSession(userID, name string) (*store.Session, error) {
	return &store.Session{ID: "new", UserID: userID, Name: name, Current: true}, nil
}

func (f *fakeConversationManager) ListSessions(userID string) ([]store.Session, error) {
	return f.sessions, nil
}

func (f *fakeConversationManager) SwitchSession(userID, sessionName string) (*store.Session, error) {
	return &store.Session{ID: "switched", UserID: userID, Name: sessionName, Current: true}, nil
}

func (f *fakeConversationManager) RenameCurrentSession(userID, newName string) (*store.Session, error) {
	return &store.Session{ID: "session", UserID: userID, Name: newName, Current: true}, nil
}

func (f *fakeConversationManager) ArchiveSession(userID, sessionName string) (*store.ArchiveResult, error) {
	return &store.ArchiveResult{
		Archived:       store.Session{ID: "session", UserID: userID, Name: "default", Archived: true},
		Current:        &store.Session{ID: "new", UserID: userID, Name: "new", Current: true},
		CurrentChanged: true,
	}, nil
}

func (f *fakeConversationManager) ClearSession(userID string) (*store.Session, error) {
	return &store.Session{ID: "cleared", UserID: userID, Name: "session-1", Current: true}, nil
}

func (f *fakeConversationManager) CurrentModel(userID, sessionID string) (string, error) {
	if f.modelBySession != nil && f.modelBySession[sessionID] != "" {
		return f.modelBySession[sessionID], nil
	}
	return "deepseek", nil
}

func (f *fakeConversationManager) SetModel(userID, sessionID, modelName string) error {
	if f.modelBySession == nil {
		f.modelBySession = map[string]string{}
	}
	f.modelBySession[sessionID] = modelName
	return nil
}

func (f *fakeConversationManager) DefaultModelName() string {
	return "deepseek"
}

func (f *fakeConversationManager) ListModels() []string {
	if len(f.models) > 0 {
		return f.models
	}
	return []string{"deepseek", "gpt4o"}
}

type fakeLLM struct {
	response            llm.Response
	err                 error
	prepareErr          error
	assistantErr        error
	called              bool
	prepareCalled       bool
	assistantCalled     bool
	preparedContent     string
	preparedAttachments []llm.InputAttachment
	assistantResp       llm.Response
	assistantMsg        store.Message
	messages            []store.Message
	started             chan struct{}
	release             chan struct{}
}

func (f *fakeLLM) PrepareUserMessage(content string, attachments []llm.InputAttachment) (store.Message, error) {
	f.prepareCalled = true
	f.preparedContent = content
	f.preparedAttachments = append([]llm.InputAttachment(nil), attachments...)
	if f.prepareErr != nil {
		return store.Message{}, f.prepareErr
	}
	msg := store.Message{Role: "user", Content: content}
	for _, attachment := range attachments {
		msg.Attachments = append(msg.Attachments, store.Attachment{
			Type:        attachment.Type,
			MIMEType:    attachment.MIMEType,
			Filename:    attachment.Filename,
			Size:        attachment.Size,
			RefProvider: "fake",
			RefType:     "prepared",
			RefID:       "prepared-image",
			LocalPath:   attachment.LocalPath,
		})
	}
	return msg, nil
}

func (f *fakeLLM) Chat(systemPrompt string, messages []store.Message) (llm.Response, error) {
	return f.response, f.err
}

func (f *fakeLLM) ChatStream(systemPrompt string, messages []store.Message, onChunk func(chunk string) error) (llm.Response, error) {
	f.called = true
	f.messages = messages
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		<-f.release
	}
	if f.err != nil {
		return llm.Response{}, f.err
	}
	if onChunk != nil && f.response.Text != "" {
		if err := onChunk(f.response.Text); err != nil {
			return llm.Response{}, err
		}
	}
	return f.response, nil
}

func (f *fakeLLM) AssistantMessage(resp llm.Response) (store.Message, error) {
	f.assistantCalled = true
	f.assistantResp = resp
	if f.assistantErr != nil {
		return store.Message{}, f.assistantErr
	}
	if f.assistantMsg.Role != "" || f.assistantMsg.Content != "" || len(f.assistantMsg.Attachments) > 0 {
		return f.assistantMsg, nil
	}
	var parts []string
	if resp.Text != "" {
		parts = append(parts, resp.Text)
	}
	for _, image := range resp.Images {
		mimeType := image.MIMEType
		if mimeType == "" {
			mimeType = "image/png"
		}
		filename := image.Filename
		if filename == "" {
			filename = "image.png"
		}
		parts = append(parts, fmt.Sprintf("[图片: mime=%s filename=%s base64=%s]", mimeType, filename, base64.StdEncoding.EncodeToString(image.Data)))
	}
	return store.Message{Role: "assistant", Content: strings.Join(parts, "\n")}, nil
}

func newTestBot() (*bot, *fakeWechatClient, *fakeConversationManager, *fakeLLM) {
	client := &fakeWechatClient{}
	sessions := &fakeConversationManager{
		sess:           &store.Session{ID: "session", UserID: "user", Name: "default", Current: true},
		conv:           &store.Conversation{},
		modelBySession: map[string]string{"session": "deepseek"},
	}
	llmClient := &fakeLLM{response: llm.Response{Text: "hello"}}
	handler := core.New(sessions, testLLMConfig())
	handler.LLMClients = map[string]llm.Client{"deepseek": llmClient}
	handler.NewLLM = func(model config.ResolvedModel) llm.Client {
		return &fakeLLM{response: llm.Response{Text: model.Name}}
	}
	return &bot{
		account: store.Account{ID: "wechat:account", Name: "wechat-bot"},
		client:  client,
		cursors: &fakeCursorStore{},
		handler: handler,
		saveMedia: func(userID, sessionID, role string, index int, mimeType string, data []byte) (*store.MediaFile, error) {
			ext := "png"
			if mimeType == "image/jpeg" {
				ext = "jpg"
			}
			filename := fmt.Sprintf("%s-%d.%s", role, index+1, ext)
			return &store.MediaFile{
				RelativePath: fmt.Sprintf("media/%s/%s/%s", userID, sessionID, filename),
				Filename:     filename,
				Size:         len(data),
			}, nil
		},
	}, client, sessions, llmClient
}

func testLLMConfig() config.LLMConfig {
	return config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {
				Provider: "openai",
				BaseURL:  "https://deepseek.test/v1",
				APIKey:   "key",
				ID:       "deepseek-chat",
				Endpoint: "chat",
			},
			"gpt4o": {
				Provider: "openai",
				BaseURL:  "https://openai.test/v1",
				APIKey:   "key",
				ID:       "gpt-4o",
				Endpoint: "responses",
			},
		},
		SystemPrompt: "system",
	}
}

func textMessage(text string) *api.WeixinMessage {
	return &api.WeixinMessage{
		FromUserID:   "user",
		ContextToken: "context",
		ItemList: []*api.MessageItem{
			{Type: api.ItemTypeText, TextItem: &api.TextItem{Text: text}},
		},
	}
}

func imageMessage(text string) *api.WeixinMessage {
	items := []*api.MessageItem{}
	if text != "" {
		items = append(items, &api.MessageItem{Type: api.ItemTypeText, TextItem: &api.TextItem{Text: text}})
	}
	items = append(items, &api.MessageItem{
		Type: api.ItemTypeImage,
		ImageItem: &api.ImageItem{
			Media: &api.CDNMedia{EncryptQueryParam: "download-param", AESKey: "aes-key", EncryptType: 1},
		},
	})
	return &api.WeixinMessage{
		FromUserID:   "user",
		ContextToken: "context",
		ItemList:     items,
	}
}

func lastSentText(t *testing.T, client *fakeWechatClient) string {
	t.Helper()
	if len(client.sent) == 0 {
		t.Fatal("no message sent")
	}
	items := client.sent[len(client.sent)-1].ItemList
	if len(items) == 0 || items[0].TextItem == nil {
		t.Fatal("last sent message is not text")
	}
	return items[0].TextItem.Text
}

func joinedSentText(t *testing.T, client *fakeWechatClient) string {
	t.Helper()
	var out strings.Builder
	for _, msg := range client.sent {
		items := msg.ItemList
		if len(items) == 0 || items[0].TextItem == nil {
			t.Fatal("sent message is not text")
		}
		out.WriteString(items[0].TextItem.Text)
	}
	return out.String()
}

func TestProcessOneTextMessage(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()

	if err := b.processOne(textMessage("hi")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if !llmClient.called {
		t.Fatal("LLM was not called")
	}
	if got := lastSentText(t, client); got != "hello" {
		t.Fatalf("sent text = %q, want hello", got)
	}
	if len(client.typing) != 2 || client.typing[0] != api.TypingStatusTyping || client.typing[1] != api.TypingStatusCancel {
		t.Fatalf("typing statuses = %v", client.typing)
	}
	if sessions.saved == nil || len(sessions.saved.Messages) != 2 {
		t.Fatalf("saved conversation = %#v, want two messages", sessions.saved)
	}
	if got := sessions.saved.Messages[0].Content; got != "hi" {
		t.Fatalf("saved user message = %q, want hi", got)
	}
}

func TestProcessOneTextMessageIncludesAccountIdentity(t *testing.T) {
	handler := &captureHandler{}
	b := &bot{
		account: store.Account{ID: "wechat:account", Name: "wechat-bot"},
		handler: handler,
	}

	if err := b.processOne(textMessage("hi")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}
	if !handler.called {
		t.Fatal("handler was not called")
	}
	if handler.msg.Platform != store.PlatformWeChat || handler.msg.AccountID != "wechat:account" || handler.msg.AccountName != "wechat-bot" {
		t.Fatalf("message scope = %#v, want wechat account identity", handler.msg)
	}
}

func TestWechatSenderCompactNoticeSendsStartAndFinishText(t *testing.T) {
	b, client, _, _ := newTestBot()
	sender := wechatSender{bot: b, toUserID: "user", contextToken: "context"}
	notice := core.CompactNotice{
		ModelName:         "deepseek",
		Manual:            true,
		CompactedMessages: 2,
		RetainedMessages:  12,
	}

	if _, err := sender.StartCompactNotice(context.Background(), notice); err != nil {
		t.Fatalf("StartCompactNotice returned error: %v", err)
	}
	if err := sender.FinishCompactNotice(context.Background(), core.CompactNoticeHandle{}, notice); err != nil {
		t.Fatalf("FinishCompactNotice returned error: %v", err)
	}

	if len(client.sent) != 2 {
		t.Fatalf("sent messages = %d, want start and finish notices", len(client.sent))
	}
	if got := client.sent[0].ItemList[0].TextItem.Text; got != core.CompactStartText() {
		t.Fatalf("start text = %q, want compact start text", got)
	}
	if got := client.sent[1].ItemList[0].TextItem.Text; got != core.CompactSuccessText(notice) {
		t.Fatalf("finish text = %q, want compact success text", got)
	}
}

func TestProcessOneImageMessageUsesPreparedAttachmentRef(t *testing.T) {
	b, _, sessions, llmClient := newTestBot()
	b.downloadImage = func(item *api.ImageItem) ([]byte, string, error) {
		return []byte("image-bytes"), "image/png", nil
	}

	if err := b.processOne(imageMessage("what is this?")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if !llmClient.called {
		t.Fatal("LLM was not called")
	}
	if !llmClient.prepareCalled {
		t.Fatal("PrepareUserMessage was not called")
	}
	if llmClient.preparedContent != "what is this?" {
		t.Fatalf("prepared content = %q, want prompt text", llmClient.preparedContent)
	}
	if len(llmClient.preparedAttachments) != 1 {
		t.Fatalf("prepared attachments = %#v, want one image attachment", llmClient.preparedAttachments)
	}
	prepared := llmClient.preparedAttachments[0]
	if prepared.Type != "image" || prepared.MIMEType != "image/png" || prepared.Filename != "user-1.png" || string(prepared.Data) != "image-bytes" {
		t.Fatalf("prepared attachment = %#v", prepared)
	}
	if prepared.LocalPath != "media/user/session/user-1.png" {
		t.Fatalf("prepared local path = %q, want media/user/session/user-1.png", prepared.LocalPath)
	}
	userMsg := llmClient.messages[len(llmClient.messages)-1]
	if userMsg.Content != "what is this?" {
		t.Fatalf("LLM user content = %q, want prompt text", userMsg.Content)
	}
	if len(userMsg.Attachments) != 1 || userMsg.Attachments[0].RefProvider != "fake" || userMsg.Attachments[0].RefType != "prepared" || userMsg.Attachments[0].RefID != "prepared-image" {
		t.Fatalf("LLM user attachments = %#v, want prepared ref", userMsg.Attachments)
	}
	if sessions.saved == nil || len(sessions.saved.Messages) < 1 {
		t.Fatalf("saved conversation = %#v, want saved image message", sessions.saved)
	}
	if got := sessions.saved.Messages[0].Attachments[0].RefID; got != "prepared-image" {
		t.Fatalf("saved ref id = %q, want prepared-image", got)
	}
	if got := sessions.saved.Messages[0].Attachments[0].LocalPath; got != "media/user/session/user-1.png" {
		t.Fatalf("saved local path = %q, want media/user/session/user-1.png", got)
	}
}

func TestProcessOneImageOnlyUsesDefaultPrompt(t *testing.T) {
	b, _, sessions, llmClient := newTestBot()
	b.downloadImage = func(item *api.ImageItem) ([]byte, string, error) {
		return []byte("image-bytes"), "image/png", nil
	}

	if err := b.processOne(imageMessage("")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if llmClient.preparedContent != defaultImagePrompt {
		t.Fatalf("prepared content = %q, want default image prompt", llmClient.preparedContent)
	}
	userMsg := llmClient.messages[len(llmClient.messages)-1]
	if userMsg.Content != defaultImagePrompt {
		t.Fatalf("LLM user content = %q, want default image prompt", userMsg.Content)
	}
	if len(userMsg.Attachments) != 1 {
		t.Fatalf("LLM user attachments = %#v, want image attachment", userMsg.Attachments)
	}
	if got := sessions.saved.Messages[0].Content; got != defaultImagePrompt {
		t.Fatalf("saved user content = %q, want default image prompt", got)
	}
}

func TestProcessOneImageUnsupportedByModel(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	b.downloadImage = func(item *api.ImageItem) ([]byte, string, error) {
		return []byte("image-bytes"), "image/png", nil
	}
	llmClient.prepareErr = llm.ErrUnsupportedAttachment

	err := b.processOne(imageMessage("what is this?"))
	if !errors.Is(err, llm.ErrUnsupportedAttachment) {
		t.Fatalf("processOne error = %v, want ErrUnsupportedAttachment", err)
	}
	if llmClient.called {
		t.Fatal("LLM was called")
	}
	if sessions.saved != nil {
		t.Fatalf("conversation was saved: %#v", sessions.saved)
	}
	if got := lastSentText(t, client); !strings.Contains(got, "不支持图片上下文") {
		t.Fatalf("sent text = %q, want unsupported image notice", got)
	}
}

func TestProcessOneLLMUnsupportedAttachmentSendsImageNotice(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.err = llm.ErrUnsupportedAttachment

	err := b.processOne(textMessage("continue"))
	if !errors.Is(err, llm.ErrUnsupportedAttachment) {
		t.Fatalf("processOne error = %v, want ErrUnsupportedAttachment", err)
	}
	if sessions.saved != nil {
		t.Fatalf("conversation was saved: %#v", sessions.saved)
	}
	if got := lastSentText(t, client); !strings.Contains(got, "不支持图片上下文") {
		t.Fatalf("sent text = %q, want unsupported image notice", got)
	}
}

func TestProcessOneSendsTextThenImage(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.response = llm.Response{
		Text:   "caption",
		Images: []llm.Image{{Data: []byte("image-bytes"), MIMEType: "image/png", Filename: "image.png"}},
	}
	b.uploadImage = func(client wechatClient, httpClient *http.Client, cdnBaseURL, toUserID string, image llm.Image) (*uploadedImage, error) {
		return &uploadedImage{
			Media:   &api.CDNMedia{EncryptQueryParam: "download-param", AESKey: "aes-key", EncryptType: 1},
			MidSize: 32,
		}, nil
	}

	if err := b.processOne(textMessage("draw")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if len(client.sent) != 2 {
		t.Fatalf("sent messages = %d, want text and image", len(client.sent))
	}
	if got := client.sent[0].ItemList[0].TextItem.Text; got != "caption" {
		t.Fatalf("sent text = %q, want caption", got)
	}
	imageItem := client.sent[1].ItemList[0].ImageItem
	if imageItem == nil || imageItem.Media == nil {
		t.Fatal("second sent message is not an image")
	}
	if imageItem.Media.EncryptQueryParam != "download-param" || imageItem.MidSize != 32 {
		t.Fatalf("image item = %#v", imageItem)
	}
	if got := sessions.saved.Messages[1].Content; got != "caption\n[图片: mime=image/png filename=assistant-1.png base64=aW1hZ2UtYnl0ZXM=]" {
		t.Fatalf("saved assistant message = %q", got)
	}
}

func TestProcessOneSavesAssistantMessageFromLLMClient(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.response = llm.Response{
		Text: "caption",
		Images: []llm.Image{{
			Data:     []byte("image-bytes"),
			MIMEType: "image/png",
			Filename: "image.png",
			Reference: llm.AttachmentRef{
				Provider: "openai",
				Type:     "image_generation_call",
				ID:       "ig_123",
			},
		}},
	}
	llmClient.assistantMsg = store.Message{
		Role:    "assistant",
		Content: "caption\n[图片: mime=image/png filename=assistant-1.png]",
		Attachments: []store.Attachment{{
			Type:        "image",
			MIMEType:    "image/png",
			Filename:    "assistant-1.png",
			Size:        len("image-bytes"),
			RefProvider: "openai",
			RefType:     "file",
			RefID:       "file_123",
			LocalPath:   "media/user/session/assistant-1.png",
		}},
	}
	b.uploadImage = func(client wechatClient, httpClient *http.Client, cdnBaseURL, toUserID string, image llm.Image) (*uploadedImage, error) {
		return &uploadedImage{
			Media:   &api.CDNMedia{EncryptQueryParam: "download-param", AESKey: "aes-key", EncryptType: 1},
			MidSize: 32,
		}, nil
	}

	if err := b.processOne(textMessage("draw")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if len(client.sent) != 2 {
		t.Fatalf("sent messages = %d, want text and image", len(client.sent))
	}
	if !llmClient.assistantCalled {
		t.Fatal("AssistantMessage was not called")
	}
	if len(llmClient.assistantResp.Images) != 1 || llmClient.assistantResp.Images[0].LocalPath != "media/user/session/assistant-1.png" {
		t.Fatalf("assistant response images = %#v, want persisted local path", llmClient.assistantResp.Images)
	}
	assistantMsg := sessions.saved.Messages[1]
	if strings.Contains(assistantMsg.Content, "base64=") {
		t.Fatalf("saved assistant content contains base64: %q", assistantMsg.Content)
	}
	if got := assistantMsg.Content; got != "caption\n[图片: mime=image/png filename=assistant-1.png]" {
		t.Fatalf("saved assistant content = %q", got)
	}
	if len(assistantMsg.Attachments) != 1 {
		t.Fatalf("assistant attachments = %#v, want one image attachment", assistantMsg.Attachments)
	}
	attachment := assistantMsg.Attachments[0]
	if attachment.RefProvider != "openai" || attachment.RefType != "file" || attachment.RefID != "file_123" {
		t.Fatalf("attachment ref = %#v, want openai file file_123", attachment)
	}
	if attachment.LocalPath != "media/user/session/assistant-1.png" {
		t.Fatalf("attachment local path = %q, want persisted path", attachment.LocalPath)
	}
}

func TestProcessOneSendsImageOnly(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.response = llm.Response{
		Images: []llm.Image{{Data: []byte("image-bytes"), MIMEType: "image/png", Filename: "image.png"}},
	}
	b.uploadImage = func(client wechatClient, httpClient *http.Client, cdnBaseURL, toUserID string, image llm.Image) (*uploadedImage, error) {
		return &uploadedImage{
			Media:   &api.CDNMedia{EncryptQueryParam: "download-param", AESKey: "aes-key", EncryptType: 1},
			MidSize: 32,
		}, nil
	}

	if err := b.processOne(textMessage("draw")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if len(client.sent) != 1 {
		t.Fatalf("sent messages = %d, want image only", len(client.sent))
	}
	if item := client.sent[0].ItemList[0]; item.Type != api.ItemTypeImage || item.ImageItem == nil {
		t.Fatalf("sent item = %#v, want image", item)
	}
	if got := sessions.saved.Messages[1].Content; got != "[图片: mime=image/png filename=assistant-1.png base64=aW1hZ2UtYnl0ZXM=]" {
		t.Fatalf("saved assistant message = %q", got)
	}
}

func TestProcessOneSendsImageWhenAssistantFileRefIsMissing(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.response = llm.Response{
		Images: []llm.Image{{Data: []byte("image-bytes"), MIMEType: "image/png", Filename: "image.png"}},
	}
	llmClient.assistantMsg = store.Message{
		Role:    "assistant",
		Content: "[图片: mime=image/png filename=assistant-1.png]",
		Attachments: []store.Attachment{{
			Type:        "image",
			MIMEType:    "image/png",
			Filename:    "assistant-1.png",
			Size:        len("image-bytes"),
			RefProvider: "openai",
			RefType:     "file",
			RefID:       "",
			LocalPath:   "media/user/session/assistant-1.png",
		}},
	}
	b.uploadImage = func(client wechatClient, httpClient *http.Client, cdnBaseURL, toUserID string, image llm.Image) (*uploadedImage, error) {
		return &uploadedImage{
			Media:   &api.CDNMedia{EncryptQueryParam: "download-param", AESKey: "aes-key", EncryptType: 1},
			MidSize: 32,
		}, nil
	}

	if err := b.processOne(textMessage("draw")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sent messages = %d, want image reply", len(client.sent))
	}
	assistantMsg := sessions.saved.Messages[1]
	if len(assistantMsg.Attachments) != 1 {
		t.Fatalf("assistant attachments = %#v, want one image attachment", assistantMsg.Attachments)
	}
	attachment := assistantMsg.Attachments[0]
	if attachment.RefProvider != "openai" || attachment.RefType != "file" || attachment.RefID != "" {
		t.Fatalf("attachment ref = %#v, want openai file with empty ref id", attachment)
	}
	if attachment.LocalPath != "media/user/session/assistant-1.png" {
		t.Fatalf("attachment local path = %q, want persisted path", attachment.LocalPath)
	}
}

func TestProcessOneGeneratedImageLocalSaveFailureStillSendsImage(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.response = llm.Response{
		Images: []llm.Image{{Data: []byte("image-bytes"), MIMEType: "image/png", Filename: "image.png"}},
	}
	b.saveMedia = func(userID, sessionID, role string, index int, mimeType string, data []byte) (*store.MediaFile, error) {
		if role == "assistant" {
			return nil, errors.New("save failed")
		}
		return &store.MediaFile{
			RelativePath: fmt.Sprintf("media/%s/%s/%s-%d.png", userID, sessionID, role, index+1),
			Filename:     fmt.Sprintf("%s-%d.png", role, index+1),
			Size:         len(data),
		}, nil
	}
	b.uploadImage = func(client wechatClient, httpClient *http.Client, cdnBaseURL, toUserID string, image llm.Image) (*uploadedImage, error) {
		return &uploadedImage{
			Media:   &api.CDNMedia{EncryptQueryParam: "download-param", AESKey: "aes-key", EncryptType: 1},
			MidSize: 32,
		}, nil
	}

	if err := b.processOne(textMessage("draw")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sent messages = %d, want image reply", len(client.sent))
	}
	if len(llmClient.assistantResp.Images) != 1 || llmClient.assistantResp.Images[0].LocalPath != "" {
		t.Fatalf("assistant response images = %#v, want empty local path after save failure", llmClient.assistantResp.Images)
	}
	if got := sessions.saved.Messages[1].Content; got != "[图片: mime=image/png filename=image.png base64=aW1hZ2UtYnl0ZXM=]" {
		t.Fatalf("saved assistant message = %q", got)
	}
}

func TestProcessOneImageUploadFailureSendsErrorNotice(t *testing.T) {
	b, client, _, llmClient := newTestBot()
	llmClient.response = llm.Response{
		Images: []llm.Image{{Data: []byte("image-bytes"), MIMEType: "image/png", Filename: "image.png"}},
	}
	b.uploadImage = func(client wechatClient, httpClient *http.Client, cdnBaseURL, toUserID string, image llm.Image) (*uploadedImage, error) {
		return nil, errors.New("upload failed")
	}

	err := b.processOne(textMessage("draw"))
	if err == nil || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("processOne error = %v, want upload failed", err)
	}
	if got := lastSentText(t, client); !strings.Contains(got, "AI 响应失败") || !strings.Contains(got, "upload failed") {
		t.Fatalf("sent text = %q, want AI error summary", got)
	}
}

func TestUploadImageToWeixinCDN(t *testing.T) {
	plain := []byte("image-bytes")
	var uploadedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Fatalf("Content-Type = %q", got)
		}
		var err error
		uploadedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}
		w.Header().Set("x-encrypted-param", "download-param")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &fakeWechatClient{
		uploadResp: &api.GetUploadUrlResp{UploadFullURL: server.URL},
	}
	uploaded, err := uploadImageToWeixinCDN(client, server.Client(), "", "user", llm.Image{Data: plain})
	if err != nil {
		t.Fatalf("uploadImageToWeixinCDN returned error: %v", err)
	}

	if client.uploadReq == nil {
		t.Fatal("GetUploadUrl was not called")
	}
	if client.uploadReq.MediaType != api.UploadMediaTypeImage || client.uploadReq.ToUserID != "user" {
		t.Fatalf("upload request = %#v", client.uploadReq)
	}
	if client.uploadReq.RawSize != len(plain) || client.uploadReq.FileSize != cdn.AESPaddedSize(len(plain)) {
		t.Fatalf("upload sizes = raw %d file %d", client.uploadReq.RawSize, client.uploadReq.FileSize)
	}
	if len(uploadedBody) != cdn.AESPaddedSize(len(plain)) {
		t.Fatalf("uploaded body len = %d, want %d", len(uploadedBody), cdn.AESPaddedSize(len(plain)))
	}
	if uploaded.Media.EncryptQueryParam != "download-param" || uploaded.Media.AESKey == "" || uploaded.Media.EncryptType != 1 {
		t.Fatalf("uploaded media = %#v", uploaded.Media)
	}
	decodedMediaAESKey, err := base64.StdEncoding.DecodeString(uploaded.Media.AESKey)
	if err != nil {
		t.Fatalf("decode media aes_key: %v", err)
	}
	if got := string(decodedMediaAESKey); got != client.uploadReq.AESKey {
		t.Fatalf("media aes_key decodes to %q, want upload aeskey hex %q", got, client.uploadReq.AESKey)
	}
	if len(decodedMediaAESKey) != 32 {
		t.Fatalf("media aes_key decoded len = %d, want 32-byte hex string", len(decodedMediaAESKey))
	}
	if uploaded.MidSize != len(uploadedBody) {
		t.Fatalf("mid size = %d, want %d", uploaded.MidSize, len(uploadedBody))
	}
}

func TestUploadImageToWeixinCDNFallsBackToUploadParamCDNURL(t *testing.T) {
	plain := []byte("image-bytes")
	client := &fakeWechatClient{
		uploadResp: &api.GetUploadUrlResp{UploadParam: "upload-param"},
	}
	var uploadedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/upload" {
			t.Fatalf("path = %q, want /upload", r.URL.Path)
		}
		if got := r.URL.Query().Get("encrypted_query_param"); got != "upload-param" {
			t.Fatalf("encrypted_query_param = %q, want upload-param", got)
		}
		if client.uploadReq == nil {
			t.Fatal("GetUploadUrl was not called before CDN upload")
		}
		if got := r.URL.Query().Get("filekey"); got == "" || got != client.uploadReq.FileKey {
			t.Fatalf("filekey = %q, want generated filekey %q", got, client.uploadReq.FileKey)
		}
		var err error
		uploadedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}
		w.Header().Set("x-encrypted-param", "download-param")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	uploaded, err := uploadImageToWeixinCDN(client, server.Client(), server.URL, "user", llm.Image{Data: plain})
	if err != nil {
		t.Fatalf("uploadImageToWeixinCDN returned error: %v", err)
	}

	if len(uploadedBody) != cdn.AESPaddedSize(len(plain)) {
		t.Fatalf("uploaded body len = %d, want %d", len(uploadedBody), cdn.AESPaddedSize(len(plain)))
	}
	if uploaded.Media.EncryptQueryParam != "download-param" || uploaded.MidSize != len(uploadedBody) {
		t.Fatalf("uploaded = %#v", uploaded)
	}
}

func TestProcessOneTypingKeepaliveRepeatsUntilLLMReturns(t *testing.T) {
	b, client, _, llmClient := newTestBot()
	b.typingTick = 5 * time.Millisecond
	client.typingCh = make(chan int, 10)
	llmClient.started = make(chan struct{})
	llmClient.release = make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- b.processOne(textMessage("hi"))
	}()

	select {
	case <-llmClient.started:
	case <-time.After(time.Second):
		t.Fatal("LLM did not start")
	}

	typingCount := 0
	for typingCount < 2 {
		select {
		case status := <-client.typingCh:
			if status == api.TypingStatusTyping {
				typingCount++
			}
		case <-time.After(time.Second):
			t.Fatalf("typing keepalive count = %d, want at least 2", typingCount)
		}
	}

	close(llmClient.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("processOne returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("processOne did not finish")
	}

	if got := client.typing[len(client.typing)-1]; got != api.TypingStatusCancel {
		t.Fatalf("last typing status = %d, want cancel", got)
	}
}

func TestProcessOneTypingErrorDoesNotBlockReply(t *testing.T) {
	b, client, _, llmClient := newTestBot()
	client.typingErr = errors.New("typing failed")

	if err := b.processOne(textMessage("hi")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if !llmClient.called {
		t.Fatal("LLM was not called")
	}
	if got := lastSentText(t, client); got != "hello" {
		t.Fatalf("sent text = %q, want hello", got)
	}
}

func TestProcessOneSendsRawMarkdownText(t *testing.T) {
	b, client, _, llmClient := newTestBot()
	llmClient.response.Text = "Commands:\n```bash\nsudo dnf update -y\n```\ninline `code`"

	if err := b.processOne(textMessage("hi")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	want := llmClient.response.Text
	if got := lastSentText(t, client); got != want {
		t.Fatalf("sent text = %q, want raw markdown %q", got, want)
	}
}

func TestWechatSenderSendsMarkdownImageSyntaxAsText(t *testing.T) {
	b, client, _, _ := newTestBot()
	sender := wechatSender{bot: b, toUserID: "user", contextToken: "context"}

	text := "![alt](https://example.test/image.png)"
	if err := sender.Send(context.Background(), core.OutboundMessage{Text: text}); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if got := lastSentText(t, client); got != text {
		t.Fatalf("sent text = %q, want raw markdown image syntax %q", got, text)
	}
}

func TestProcessOneLongReplyIsChunked(t *testing.T) {
	b, client, _, llmClient := newTestBot()
	llmClient.response.Text = strings.Repeat("甲", TextChunkLimit+50)

	if err := b.processOne(textMessage("hi")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if len(client.sent) != 2 {
		t.Fatalf("sent messages = %d, want 2", len(client.sent))
	}
	if got := joinedSentText(t, client); got != llmClient.response.Text {
		t.Fatalf("joined sent text length = %d, want exact response length %d", len(got), len(llmClient.response.Text))
	}
	for i, msg := range client.sent {
		text := msg.ItemList[0].TextItem.Text
		if got := len([]rune(text)); got > TextChunkLimit {
			t.Fatalf("chunk %d rune length = %d, want <= %d", i+1, got, TextChunkLimit)
		}
	}
}

func TestProcessOneReturnsErrorWhenReplyChunkSendFails(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.response.Text = strings.Repeat("x", TextChunkLimit+1)
	client.failSendAt = 2
	client.sendErr = errors.New("send failed")

	err := b.processOne(textMessage("hi"))
	if err == nil || !strings.Contains(err.Error(), "send failed") {
		t.Fatalf("processOne error = %v, want send failed", err)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sent messages = %d, want first chunk only", len(client.sent))
	}
	if sessions.saved == nil || len(sessions.saved.Messages) != 2 {
		t.Fatalf("saved conversation = %#v, want complete assistant history saved before send", sessions.saved)
	}
}

func TestSplitTextChunksPreservesMultibyteText(t *testing.T) {
	text := "你好🙂世界\n" + strings.Repeat("再见🙂 ", 5)
	chunks := core.SplitTextChunksByRunes(text, 7)

	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want multiple chunks", len(chunks))
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8: %q", i+1, chunk)
		}
		if got := len([]rune(chunk)); got > 7 {
			t.Fatalf("chunk %d rune length = %d, want <= 7", i+1, got)
		}
	}
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("joined chunks = %q, want %q", got, text)
	}
}

func TestProcessOneSlashCommand(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	sessions.sessions = []store.Session{
		{ID: "session", UserID: "user", Name: "default", Current: true},
	}

	if err := b.processOne(textMessage("/list")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if llmClient.called {
		t.Fatal("LLM was called for slash command")
	}
	if got := lastSentText(t, client); !strings.Contains(got, "default") {
		t.Fatalf("sent text = %q, want session list", got)
	}
}

func TestProcessOneSlashCommandIncludesAccountIdentity(t *testing.T) {
	handler := &captureHandler{}
	b := &bot{
		account: store.Account{ID: "wechat:account", Name: "wechat-bot"},
		handler: handler,
	}

	if err := b.processOne(textMessage("/list")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}
	if !handler.called {
		t.Fatal("handler was not called")
	}
	if handler.msg.Platform != store.PlatformWeChat || handler.msg.AccountID != "wechat:account" || handler.msg.AccountName != "wechat-bot" {
		t.Fatalf("message scope = %#v, want wechat account identity", handler.msg)
	}
}

func TestProcessOneQuotedTextMessage(t *testing.T) {
	b, _, sessions, llmClient := newTestBot()
	msg := &api.WeixinMessage{
		FromUserID:   "user",
		ContextToken: "context",
		ItemList: []*api.MessageItem{
			{
				Type:     api.ItemTypeText,
				TextItem: &api.TextItem{Text: "what about this?"},
				RefMsg: &api.RefMessage{MessageItem: &api.MessageItem{
					Type:     api.ItemTypeText,
					TextItem: &api.TextItem{Text: "original text"},
				}},
			},
		},
	}

	if err := b.processOne(msg); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	want := "[引用: original text]\nwhat about this?"
	if !llmClient.called {
		t.Fatal("LLM was not called")
	}
	if got := llmClient.messages[len(llmClient.messages)-1].Content; got != want {
		t.Fatalf("LLM user message = %q, want %q", got, want)
	}
	if got := sessions.saved.Messages[0].Content; got != want {
		t.Fatalf("saved user message = %q, want %q", got, want)
	}
}

func TestProcessOneQuotedSlashCommandUsesPlainText(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	sessions.sessions = []store.Session{
		{ID: "session", UserID: "user", Name: "default", Current: true},
	}
	msg := &api.WeixinMessage{
		FromUserID:   "user",
		ContextToken: "context",
		ItemList: []*api.MessageItem{
			{
				Type:     api.ItemTypeText,
				TextItem: &api.TextItem{Text: "/list"},
				RefMsg: &api.RefMessage{MessageItem: &api.MessageItem{
					Type:     api.ItemTypeText,
					TextItem: &api.TextItem{Text: "quoted"},
				}},
			},
		},
	}

	if err := b.processOne(msg); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if llmClient.called {
		t.Fatal("LLM was called for slash command")
	}
	if got := lastSentText(t, client); !strings.Contains(got, "default") {
		t.Fatalf("sent text = %q, want session list", got)
	}
}

func TestProcessOneVoiceTranscription(t *testing.T) {
	b, _, sessions, llmClient := newTestBot()
	msg := &api.WeixinMessage{
		FromUserID:   "user",
		ContextToken: "context",
		ItemList: []*api.MessageItem{
			{Type: api.ItemTypeVoice, VoiceItem: &api.VoiceItem{Text: "voice text"}},
		},
	}

	if err := b.processOne(msg); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if !llmClient.called {
		t.Fatal("LLM was not called")
	}
	if got := sessions.saved.Messages[0].Content; got != "voice text" {
		t.Fatalf("saved user message = %q, want voice text", got)
	}
}

func TestProcessOneUnsupportedFile(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	msg := &api.WeixinMessage{
		FromUserID:   "user",
		ContextToken: "context",
		ItemList: []*api.MessageItem{
			{Type: api.ItemTypeFile, FileItem: &api.FileItem{FileName: "a.txt"}},
		},
	}

	if err := b.processOne(msg); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if llmClient.called {
		t.Fatal("LLM was called for unsupported file")
	}
	if sessions.saved != nil {
		t.Fatalf("conversation was saved: %#v", sessions.saved)
	}
	if got := lastSentText(t, client); !strings.Contains(got, "文件消息") {
		t.Fatalf("sent text = %q, want file unsupported message", got)
	}
}

func TestProcessOnePrepareUserMessageErrorSendsSummary(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.prepareErr = errors.New("prepare failed")

	err := b.processOne(textMessage("hi"))
	if err == nil || !strings.Contains(err.Error(), "prepare failed") {
		t.Fatalf("processOne error = %v, want prepare failed", err)
	}
	if llmClient.called {
		t.Fatal("LLM was called after prepare failure")
	}
	if sessions.saved != nil {
		t.Fatalf("conversation was saved: %#v", sessions.saved)
	}
	if got := lastSentText(t, client); !strings.Contains(got, "AI 响应失败") || !strings.Contains(got, "prepare failed") {
		t.Fatalf("sent text = %q, want AI error summary", got)
	}
}

func TestProcessOneLLMError(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.err = errors.New("boom")

	err := b.processOne(textMessage("hi"))
	if err == nil {
		t.Fatal("processOne returned nil error")
	}
	if sessions.saved != nil {
		t.Fatalf("conversation was saved: %#v", sessions.saved)
	}
	if got := lastSentText(t, client); !strings.Contains(got, "AI 响应失败") || !strings.Contains(got, "boom") {
		t.Fatalf("sent text = %q, want AI error summary", got)
	}
	if len(client.typing) != 2 || client.typing[len(client.typing)-1] != api.TypingStatusCancel {
		t.Fatalf("typing statuses = %v, want typing then cancel", client.typing)
	}
}

func TestProcessOneAssistantMessageErrorSendsSummary(t *testing.T) {
	b, client, sessions, llmClient := newTestBot()
	llmClient.response = llm.Response{Text: "ok"}
	llmClient.assistantErr = errors.New("assistant history failed")

	err := b.processOne(textMessage("hi"))
	if err == nil || !strings.Contains(err.Error(), "assistant history failed") {
		t.Fatalf("processOne error = %v, want assistant history failed", err)
	}
	if sessions.saved != nil {
		t.Fatalf("conversation was saved: %#v", sessions.saved)
	}
	if got := lastSentText(t, client); !strings.Contains(got, "AI 响应失败") || !strings.Contains(got, "assistant history failed") {
		t.Fatalf("sent text = %q, want AI error summary", got)
	}
}

func TestAIErrorNoticeIncludesFlattenedSummary(t *testing.T) {
	got := aiErrorNotice(errors.New("responses HTTP 500:\n{\"error\":\"bad gateway\"}"))
	if !strings.HasPrefix(got, "❌ AI 响应失败：") {
		t.Fatalf("notice = %q, want AI error prefix", got)
	}
	if !strings.Contains(got, `responses HTTP 500: {"error":"bad gateway"}`) {
		t.Fatalf("notice = %q, want flattened error", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("notice contains newline: %q", got)
	}
}

func TestAIErrorNoticeRedactsSecrets(t *testing.T) {
	got := aiErrorNotice(errors.New("Authorization Bearer abc.def token sk-testSECRET123 hex ea65809c20f4c92c1e090272a2e2606cd38803d83001d6b767dcc2452c1a49e6"))
	if strings.Contains(got, "abc.def") || strings.Contains(got, "sk-testSECRET123") || strings.Contains(got, "ea65809c") {
		t.Fatalf("notice leaked secret: %q", got)
	}
	if !strings.Contains(got, "Bearer [REDACTED]") || !strings.Contains(got, "sk-[REDACTED]") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("notice = %q, want redaction markers", got)
	}
}

func TestAIErrorNoticeTruncatesLongErrors(t *testing.T) {
	got := aiErrorNotice(errors.New(strings.Repeat("界", core.AIErrorSummaryRunes+20)))
	prefix := "❌ AI 响应失败："
	body := strings.TrimPrefix(got, prefix)
	if !strings.HasSuffix(body, "...") {
		t.Fatalf("notice = %q, want ellipsis", got)
	}
	if len([]rune(strings.TrimSuffix(body, "..."))) != core.AIErrorSummaryRunes {
		t.Fatalf("summary rune len = %d, want %d", len([]rune(strings.TrimSuffix(body, "..."))), core.AIErrorSummaryRunes)
	}
}

func TestProcessOneUsesSessionModelPreference(t *testing.T) {
	b, client, sessions, defaultLLM := newTestBot()
	preferredLLM := &fakeLLM{response: llm.Response{Text: "from gpt4o"}}
	sessions.modelBySession["session"] = "gpt4o"
	b.handler.(*core.Bot).NewLLM = func(model config.ResolvedModel) llm.Client {
		if model.Name != "gpt4o" {
			t.Fatalf("created model = %s, want gpt4o", model.Name)
		}
		return preferredLLM
	}

	if err := b.processOne(textMessage("hi")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if defaultLLM.called {
		t.Fatal("default LLM was called")
	}
	if !preferredLLM.called {
		t.Fatal("preferred LLM was not called")
	}
	if got := lastSentText(t, client); got != "from gpt4o" {
		t.Fatalf("sent text = %q, want preferred response", got)
	}
}

func TestProcessOneFallsBackForUnknownSessionModel(t *testing.T) {
	b, _, sessions, defaultLLM := newTestBot()
	sessions.modelBySession["session"] = "missing"

	if err := b.processOne(textMessage("hi")); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if !defaultLLM.called {
		t.Fatal("default LLM was not called for unknown model")
	}
}

func TestRunAccountStopsOnContextCancel(t *testing.T) {
	b, client, _, _ := newTestBot()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := b.runAccount(ctx, store.Account{ID: "account", Name: "bot"})
	if err != nil {
		t.Fatalf("runAccount returned error: %v", err)
	}
	if client.stops != 1 {
		t.Fatalf("NotifyStop calls = %d, want 1", client.stops)
	}
}

func TestRunAccountCancelsDuringGetUpdates(t *testing.T) {
	client := &blockingWechatClient{ready: make(chan struct{})}
	b := &bot{
		client:  client,
		cursors: &fakeCursorStore{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- b.runAccount(ctx, store.Account{ID: "account", Name: "bot"})
	}()

	<-client.ready
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runAccount returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runAccount did not exit after cancel")
	}
	if client.stops != 1 {
		t.Fatalf("NotifyStop calls = %d, want 1", client.stops)
	}
}

type blockingWechatClient struct {
	ready chan struct{}
	stops int
}

func (b *blockingWechatClient) GetUpdatesContext(ctx context.Context, buf string) (*api.GetUpdatesResp, error) {
	close(b.ready)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingWechatClient) SendMessage(msg *api.WeixinMessage) error { return nil }

func (b *blockingWechatClient) GetUploadUrl(req *api.GetUploadUrlReq) (*api.GetUploadUrlResp, error) {
	return &api.GetUploadUrlResp{}, nil
}

func (b *blockingWechatClient) GetConfig(ilinkUserID, contextToken string) (*api.GetConfigResp, error) {
	return &api.GetConfigResp{}, nil
}

func (b *blockingWechatClient) SendTyping(ilinkUserID, typingTicket string, status int) error {
	return nil
}

func (b *blockingWechatClient) NotifyStart() error { return nil }
func (b *blockingWechatClient) NotifyStop() error {
	b.stops++
	return nil
}
