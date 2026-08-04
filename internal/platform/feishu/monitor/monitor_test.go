package monitor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lingobridge/internal/commands"
	"lingobridge/internal/core"
	"lingobridge/internal/logging"
	"lingobridge/internal/platform/feishu"
	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const testBotOpenID = "ou_bot"

type fakeProcessor struct {
	mu          sync.Mutex
	platform    string
	accountID   string
	accountName string
	userID      string
	text        string
	commandText string
	metadata    map[string]string
	actor       feishutools.Actor
	actorOK     bool
	chat        feishutools.ChatContext
	chatOK      bool
	execution   tooltypes.ExecutionContext
	executionOK bool
	tools       int
	called      bool
	calls       int
	started     chan struct{}
	release     chan struct{}
}

type fakeProcessorSnapshot struct {
	platform    string
	accountID   string
	accountName string
	userID      string
	text        string
	commandText string
	metadata    map[string]string
	actor       feishutools.Actor
	actorOK     bool
	chat        feishutools.ChatContext
	chatOK      bool
	execution   tooltypes.ExecutionContext
	executionOK bool
	tools       int
	called      bool
	calls       int
}

func (f *fakeProcessor) Handle(ctx context.Context, msg core.InboundMessage, sender core.Sender) error {
	actor, actorOK := feishutools.ActorFromContext(ctx)
	chat, chatOK := feishutools.ChatContextFromContext(ctx)
	execution, executionOK := tooltypes.ExecutionContextFromContext(ctx)
	f.mu.Lock()
	f.called = true
	f.platform = msg.Platform
	f.accountID = msg.AccountID
	f.accountName = msg.AccountName
	f.userID = msg.UserKey
	f.text = msg.LLMText
	f.commandText = msg.CommandText
	f.metadata = msg.Metadata
	f.actor = actor
	f.actorOK = actorOK
	f.chat = chat
	f.chatOK = chatOK
	f.execution = execution
	f.executionOK = executionOK
	f.tools = len(msg.Tools)
	f.calls++
	started := f.started
	release := f.release
	f.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return sender.Send(ctx, core.OutboundMessage{Text: "ok"})
}

type fakeCoreTool struct{}

func (fakeCoreTool) Spec() tooltypes.Spec {
	return tooltypes.Spec{Name: "fake_tool"}
}

func (fakeCoreTool) Execute(ctx context.Context, call tooltypes.Call) tooltypes.Result {
	return tooltypes.Result{CallID: call.ID, Name: call.Name, Content: "ok"}
}

type fakeToolApprovalRequester struct{}

func (fakeToolApprovalRequester) CheckOrRequest(context.Context, feishutools.OperationApprovalRequest) (feishutools.OperationApprovalResult, error) {
	return feishutools.OperationApprovalResult{Status: feishutools.OperationApprovalStatusPending, RequestID: "req_approval"}, nil
}

type fakeResourceAccessController struct{}

func (fakeResourceAccessController) RequestAccess(context.Context, feishutools.ResourceAccessRequest) (feishutools.ResourceAccessResult, error) {
	return feishutools.ResourceAccessResult{RequestID: "req_access", Status: feishutools.ResourceAccessStatusGranted}, nil
}

func (fakeResourceAccessController) Require(ctx context.Context, requirement feishutools.ResourceAccessRequirement) (feishutools.AuthorizedResource, error) {
	actor, _ := feishutools.ActorFromContext(ctx)
	chat, _ := feishutools.ChatContextFromContext(ctx)
	return feishutools.AuthorizedResource{
		AccountID:             "feishu:cli_test",
		ActorOpenID:           actor.OpenID,
		ActorUserID:           actor.UserID,
		ChatID:                chat.ChatID,
		ResourceType:          requirement.ResourceType,
		ResourceToken:         requirement.ResourceToken,
		EffectivePermission:   requirement.Permission,
		GrantMode:             feishutools.ResourceAccessGrantModeAll,
		CapabilitySubjectType: "bot",
		CapabilitySubjectID:   "ou_bot",
		Source:                feishutools.ResourceAccessSourceExistingGrant,
	}, nil
}

type sentText struct {
	chatID string
	text   string
}

type replyText struct {
	messageID string
	text      string
}

type updatedText struct {
	messageID string
	text      string
}

type reactionAdd struct {
	messageID string
	emojiType string
}

type reactionDelete struct {
	messageID  string
	reactionID string
}

type fakeSender struct {
	mu                sync.Mutex
	chatID            string
	text              string
	called            bool
	messages          []sentText
	streamCreates     []sentText
	replyCreates      []replyText
	streamUpdates     []updatedText
	reactionAdds      []reactionAdd
	reactionDeletes   []reactionDelete
	createTextErr     error
	addReactionErr    error
	deleteReactionErr error
	updateTextErr     error
}

type blockingSendTextSender struct {
	*fakeSender
	started chan struct{}
	release chan struct{}
}

func (s *blockingSendTextSender) SendText(ctx context.Context, chatID, text string) error {
	close(s.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return s.fakeSender.SendText(ctx, chatID, text)
	}
}

type fakeSenderSnapshot struct {
	chatID          string
	text            string
	called          bool
	messages        []sentText
	streamCreates   []sentText
	replyCreates    []replyText
	streamUpdates   []updatedText
	reactionAdds    []reactionAdd
	reactionDeletes []reactionDelete
}

type fakeClock struct {
	t time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1000, 0)}
}

func (c *fakeClock) now() time.Time {
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func (f *fakeSender) SendText(ctx context.Context, chatID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.chatID = chatID
	f.text = text
	f.messages = append(f.messages, sentText{chatID: chatID, text: text})
	return nil
}

func (f *fakeSender) CreateText(ctx context.Context, chatID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamCreates = append(f.streamCreates, sentText{chatID: chatID, text: text})
	if f.createTextErr != nil {
		return "", f.createTextErr
	}
	return "om_stream", nil
}

func (f *fakeSender) CreateReplyText(ctx context.Context, replyToMessageID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replyCreates = append(f.replyCreates, replyText{messageID: replyToMessageID, text: text})
	if f.createTextErr != nil {
		return "", f.createTextErr
	}
	return "om_reply", nil
}

func (f *fakeSender) UpdateText(ctx context.Context, messageID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamUpdates = append(f.streamUpdates, updatedText{messageID: messageID, text: text})
	return f.updateTextErr
}

func (f *fakeSender) AddReaction(ctx context.Context, messageID, emojiType string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionAdds = append(f.reactionAdds, reactionAdd{messageID: messageID, emojiType: emojiType})
	if f.addReactionErr != nil {
		return "", f.addReactionErr
	}
	return "reaction-1", nil
}

func (f *fakeSender) DeleteReaction(ctx context.Context, messageID, reactionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionDeletes = append(f.reactionDeletes, reactionDelete{messageID: messageID, reactionID: reactionID})
	return f.deleteReactionErr
}

type fakeSDKLogger struct {
	mu     sync.Mutex
	debugs int
	infos  int
	warns  int
	errors int
}

func (l *fakeSDKLogger) Debug(context.Context, ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs++
}

func (l *fakeSDKLogger) Info(context.Context, ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos++
}

func (l *fakeSDKLogger) Warn(context.Context, ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns++
}

func (l *fakeSDKLogger) Error(context.Context, ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors++
}

func (f *fakeProcessor) snapshot() fakeProcessorSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeProcessorSnapshot{
		platform:    f.platform,
		accountID:   f.accountID,
		accountName: f.accountName,
		userID:      f.userID,
		text:        f.text,
		commandText: f.commandText,
		metadata:    f.metadata,
		actor:       f.actor,
		actorOK:     f.actorOK,
		chat:        f.chat,
		chatOK:      f.chatOK,
		execution:   f.execution,
		executionOK: f.executionOK,
		tools:       f.tools,
		called:      f.called,
		calls:       f.calls,
	}
}

func (f *fakeSender) snapshot() fakeSenderSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	messages := append([]sentText(nil), f.messages...)
	streamCreates := append([]sentText(nil), f.streamCreates...)
	replyCreates := append([]replyText(nil), f.replyCreates...)
	streamUpdates := append([]updatedText(nil), f.streamUpdates...)
	reactionAdds := append([]reactionAdd(nil), f.reactionAdds...)
	reactionDeletes := append([]reactionDelete(nil), f.reactionDeletes...)
	return fakeSenderSnapshot{
		chatID:          f.chatID,
		text:            f.text,
		called:          f.called,
		messages:        messages,
		streamCreates:   streamCreates,
		replyCreates:    replyCreates,
		streamUpdates:   streamUpdates,
		reactionAdds:    reactionAdds,
		reactionDeletes: reactionDeletes,
	}
}

func waitForProcessorCalls(t *testing.T, processor *fakeProcessor, want int) fakeProcessorSnapshot {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap := processor.snapshot()
		if snap.calls >= want {
			return snap
		}
		select {
		case <-deadline:
			t.Fatalf("processor calls = %d, want at least %d", snap.calls, want)
		case <-ticker.C:
		}
	}
}

func waitForSentMessages(t *testing.T, sender *fakeSender, want int) fakeSenderSnapshot {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap := sender.snapshot()
		if len(snap.messages) >= want {
			return snap
		}
		select {
		case <-deadline:
			t.Fatalf("sent messages = %d, want at least %d", len(snap.messages), want)
		case <-ticker.C:
		}
	}
}

func waitForReplyCreates(t *testing.T, sender *fakeSender, want int) fakeSenderSnapshot {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap := sender.snapshot()
		if len(snap.replyCreates) >= want {
			return snap
		}
		select {
		case <-deadline:
			t.Fatalf("reply creates = %d, want at least %d", len(snap.replyCreates), want)
		case <-ticker.C:
		}
	}
}

func waitForReactionAdds(t *testing.T, sender *fakeSender, want int) fakeSenderSnapshot {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap := sender.snapshot()
		if len(snap.reactionAdds) >= want {
			return snap
		}
		select {
		case <-deadline:
			t.Fatalf("reaction adds = %d, want at least %d", len(snap.reactionAdds), want)
		case <-ticker.C:
		}
	}
}

func waitForReactionDeletes(t *testing.T, sender *fakeSender, want int) fakeSenderSnapshot {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap := sender.snapshot()
		if len(snap.reactionDeletes) >= want {
			return snap
		}
		select {
		case <-deadline:
			t.Fatalf("reaction deletes = %d, want at least %d", len(snap.reactionDeletes), want)
		case <-ticker.C:
		}
	}
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureMonitorLogs(t *testing.T) *synchronizedBuffer {
	t.Helper()
	base := logging.Shared()
	originalWriter := base.Writer()
	originalFlags := base.Flags()
	originalPrefix := base.Prefix()
	originalLevel := logging.GetLevel()
	t.Cleanup(func() {
		base.SetOutput(originalWriter)
		base.SetFlags(originalFlags)
		base.SetPrefix(originalPrefix)
		logging.SetLevel(originalLevel)
	})

	buf := &synchronizedBuffer{}
	base.SetOutput(buf)
	base.SetFlags(0)
	base.SetPrefix("")
	return buf
}

func TestNormalizeP2PTextMessage(t *testing.T) {
	in, ok := normalizeEvent(context.Background(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.UserID != "feishu:ou_user" || in.ChatID != "oc_chat" || in.MessageID != "om_message" || in.ReplyToMessageID != "" || in.Text != "hi" || in.Unsupported {
		t.Fatalf("incoming = %#v", in)
	}
}

func TestNormalizeGroupMessageWithoutMentionMetadataUsesGroupKey(t *testing.T) {
	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "text", `{"text":"hi"}`, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.UserID != "feishu:group:oc_chat" || in.ReplyToMessageID != "om_message" || in.Text != "hi" || !in.IsGroup || in.MentionBot {
		t.Fatalf("incoming = %#v", in)
	}
}

func TestNormalizeGroupMentionStripsMentionKey(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_bot_1", "bot", testBotOpenID, "", "LingoBridge"),
		feishuMentionWithName("@_user_1", "user", "ou_alice", "", "Alice"),
	}
	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "text", `{"text":"@_bot_1 hello @_user_1"}`, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.UserID != "feishu:group:oc_chat" || in.Text != "hello @Alice" || !in.MentionBot {
		t.Fatalf("incoming = %#v", in)
	}
	assertMentions(t, in.Mentions, feishuMention{Key: "@_user_1", Name: "Alice", OpenID: "ou_alice"})
}

func TestNormalizeTextAllMembersMentionWithoutMetadataMarksMentionAll(t *testing.T) {
	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "text", `{"text":"@_all 现在呢"}`, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if !in.MentionAll {
		t.Fatalf("incoming = %#v, want MentionAll from raw text token", in)
	}
}

func TestNormalizeTextAllMembersMentionInMiddleMarksMentionAll(t *testing.T) {
	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "text", `{"text":"hello @_all now"}`, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if !in.MentionAll {
		t.Fatalf("incoming = %#v, want MentionAll from raw text token in middle", in)
	}
}

func TestNormalizeTextMentionMetadataDeduplicatesByOpenID(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_user_1", "user", "ou_alice", "", "Alice"),
		feishuMentionWithName("@_user_2", "user", "ou_alice", "", "Alice"),
	}
	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "text", `{"text":"@_user_1 and @_user_2"}`, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.Text != "@Alice and @Alice" {
		t.Fatalf("incoming text = %q, want both mention keys replaced", in.Text)
	}
	assertMentions(t, in.Mentions, feishuMention{Key: "@_user_1", Name: "Alice", OpenID: "ou_alice"})
}

func TestNormalizeTextMentionMetadataDeduplicatesByUserID(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_user_1", "user", "", "alice_user_id", "Alice"),
		feishuMentionWithName("@_user_2", "user", "", "alice_user_id", "Alice"),
	}
	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "text", `{"text":"@_user_1 and @_user_2"}`, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.Text != "@Alice and @Alice" {
		t.Fatalf("incoming text = %q, want both mention keys replaced", in.Text)
	}
	assertMentions(t, in.Mentions, feishuMention{Key: "@_user_1", Name: "Alice", UserID: "alice_user_id"})
}

func TestNormalizeGroupMentionKeepsOtherBotMentionKey(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionEvent("@_bot_1", "bot", "ou_other_bot"),
	}
	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "text", `{"text":"@_bot_1 /help"}`, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.Text != "@_bot_1 /help" {
		t.Fatalf("incoming = %#v", in)
	}
}

func TestNormalizeGroupMessagesShareChatUserKey(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionEvent("@_bot_1", "app", testBotOpenID),
	}
	first, ok := normalizeEvent(context.Background(), feishuEventWithSender("group", "text", `{"text":"@_bot_1 first"}`, mentions, "ou_user_one"), testBotOpenID)
	if !ok {
		t.Fatal("first normalizeEvent returned ok=false")
	}
	second, ok := normalizeEvent(context.Background(), feishuEventWithSender("group", "text", `{"text":"@_bot_1 second"}`, mentions, "ou_user_two"), testBotOpenID)
	if !ok {
		t.Fatal("second normalizeEvent returned ok=false")
	}
	if first.UserID != "feishu:group:oc_chat" || second.UserID != "feishu:group:oc_chat" {
		t.Fatalf("group user keys = %q/%q, want shared chat key", first.UserID, second.UserID)
	}
	if first.Text != "first" || second.Text != "second" {
		t.Fatalf("group texts = %q/%q, want bot app mention stripped", first.Text, second.Text)
	}
}

func TestNormalizePostMessageConvertsTitleAndParagraphsToMarkdown(t *testing.T) {
	content := `{
		"title":"Bug report",
		"content":[
			[{"tag":"text","text":"Summary","style":["bold"]}],
			[{"tag":"text","text":"Body line"}],
			[],
			[{"tag":"text","text":"Tail"}]
		]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("p2p", "post", content, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	want := "# Bug report\n\n**Summary**\nBody line\n\nTail"
	if in.UserID != "feishu:ou_user" || in.Text != want || in.Unsupported {
		t.Fatalf("incoming = %#v, want text %q", in, want)
	}
}

func TestNormalizePostMessageReplacesMentionsInTitle(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_bot_1", "bot", testBotOpenID, "", "LingoBridge"),
		feishuMentionWithName("@_user_1", "user", "ou_alice", "", "Alice"),
	}
	content := `{
		"title":"@_bot_1 for @_user_1",
		"content":[
			[{"tag":"text","text":"Body"}]
		]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "post", content, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	want := "# for @Alice\n\nBody"
	if in.Text != want || in.Unsupported {
		t.Fatalf("incoming = %#v, want text %q", in, want)
	}
	assertMentions(t, in.Mentions, feishuMention{Key: "@_user_1", Name: "Alice", OpenID: "ou_alice"})
}

func TestNormalizePostMessageConvertsRichElementsToMarkdown(t *testing.T) {
	content := `{
		"content":[
			[
				{"tag":"text","text":"bold","style":["bold"]},
				{"tag":"text","text":" "},
				{"tag":"text","text":"italic","style":["italic"]},
				{"tag":"text","text":" "},
				{"tag":"text","text":"strike","style":["lineThrough"]},
				{"tag":"text","text":" "},
				{"tag":"text","text":"under","style":["underline"]},
				{"tag":"text","text":" "},
				{"tag":"a","text":"site","href":"https://example.com"}
			],
			[{"tag":"hr"}],
			[{"tag":"code_block","language":"go","content":"fmt.Println(\"hi\")"}],
			[{"tag":"img"}],
			[{"tag":"media"}],
			[{"tag":"file"}],
			[{"tag":"emotion","emoji_type":"OK"}],
			[{"tag":"widget"}]
		]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("p2p", "post", content, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	want := "**bold** *italic* ~~strike~~ <u>under</u> [site](https://example.com)\n---\n```go\nfmt.Println(\"hi\")\n```\n[图片]\n[视频]\n[文件]\n[表情:OK]\n[富文本元素:widget]"
	if in.Text != want || in.Unsupported {
		t.Fatalf("incoming = %#v, want text %q", in, want)
	}
}

func TestNormalizePostMessageHandlesMentions(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionEvent("@_bot_1", "app", testBotOpenID),
	}
	content := `{
		"content":[[
			{"tag":"text","text":"@_bot_1 hello "},
			{"tag":"at","user_name":"Alice","open_id":"ou_alice"},
			{"tag":"at","user_name":"LingoBridge","mentioned_type":"bot","open_id":"ou_bot"}
		]]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "post", content, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.UserID != "feishu:group:oc_chat" || in.Text != "hello @Alice" || in.Unsupported {
		t.Fatalf("incoming = %#v", in)
	}
	if !in.MentionBot {
		t.Fatalf("incoming = %#v, want MentionBot", in)
	}
	assertMentions(t, in.Mentions, feishuMention{Name: "Alice", OpenID: "ou_alice"})
}

func TestNormalizePostBotAtElementMarksMentionBot(t *testing.T) {
	content := `{
		"content":[[
			{"tag":"at","user_name":"LingoBridge","open_id":"ou_bot"},
			{"tag":"text","text":" hello"}
		]]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "post", content, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if !in.MentionBot || in.Text != "hello" || in.Unsupported {
		t.Fatalf("incoming = %#v, want bot mention removed and marked", in)
	}
}

func TestNormalizePostMentionMetadataDeduplicatesEventAndElement(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_user_1", "user", "ou_alice", "", "Alice"),
	}
	content := `{
		"content":[[
			{"tag":"text","text":"@_user_1 and "},
			{"tag":"at","user_name":"Alice","open_id":"ou_alice"}
		]]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "post", content, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.Text != "@Alice and @Alice" || in.Unsupported {
		t.Fatalf("incoming = %#v", in)
	}
	assertMentions(t, in.Mentions, feishuMention{Key: "@_user_1", Name: "Alice", OpenID: "ou_alice"})
}

func TestNormalizePostMentionMetadataMergesSameKeyWithElementIdentity(t *testing.T) {
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_user_1", "user", "", "", "Alice"),
	}
	content := `{
		"content":[[
			{"tag":"at","key":"@_user_1","user_name":"Alice","open_id":"ou_alice"}
		]]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "post", content, mentions), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.Text != "@Alice" || in.Unsupported {
		t.Fatalf("incoming = %#v", in)
	}
	assertMentions(t, in.Mentions, feishuMention{Key: "@_user_1", Name: "Alice", OpenID: "ou_alice"})
}

func TestNormalizePostMessageKeepsOtherBotMention(t *testing.T) {
	content := `{
		"content":[[
			{"tag":"at","user_name":"OtherBot","mentioned_type":"bot","open_id":"ou_other_bot"}
		]]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "post", content, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if in.Text != "@OtherBot" || in.Unsupported {
		t.Fatalf("incoming = %#v", in)
	}
}

func TestNormalizePostAllMembersMentionMarksMentionAll(t *testing.T) {
	content := `{
		"title":"测试富文本",
		"content":[[
			{"tag":"text","text":"富文本呢","style":["bold"]}
		],[
			{"tag":"at","user_id":"@_all","user_name":"所有人","style":[]},
			{"tag":"text","text":"","style":[]}
		]]
	}`

	in, ok := normalizeEvent(context.Background(), feishuEvent("group", "post", content, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if !in.MentionAll || in.Text != "# 测试富文本\n\n**富文本呢**" || in.Unsupported {
		t.Fatalf("incoming = %#v, want all-members mention marked and removed from text", in)
	}
}

func TestNormalizeMalformedPostMarksUnsupported(t *testing.T) {
	in, ok := normalizeEvent(context.Background(), feishuEvent("p2p", "post", `{`, nil), testBotOpenID)
	if !ok {
		t.Fatal("normalizeEvent returned ok=false")
	}
	if !in.Unsupported || in.Text != "" {
		t.Fatalf("incoming = %#v, want unsupported post", in)
	}
}

func TestHandleMessageLogsReceivedMetadata(t *testing.T) {
	buf := captureMonitorLogs(t)
	logging.SetLevel(logging.Info)
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}

	if err := b.handleMessage(context.Background(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	got := buf.String()
	for _, want := range []string{
		"[INFO] - [feishu] received feishu message",
		"chat=oc_chat",
		"user=ou_user",
		"message=om_message",
		"type=text",
		"chat_type=p2p",
		"event=event_message",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log output = %q, want %q", got, want)
		}
	}
}

func TestHandleAllMembersMentionSkipsMessage(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, botOpenID: testBotOpenID}
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_bot_1", "bot", testBotOpenID, "", "LingoBridge"),
		feishuMentionWithName("@_all_1", "all", "", "", "所有人"),
	}

	if err := b.handleMessage(context.Background(), feishuEvent("group", "text", `{"text":"@_bot_1 @_all_1 hello"}`, mentions)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	if processor.snapshot().called {
		t.Fatal("processor was called for all-members mention")
	}
	snap := sender.snapshot()
	if snap.called || len(snap.messages) != 0 || len(snap.replyCreates) != 0 || len(snap.reactionAdds) != 0 {
		t.Fatalf("sender = %#v, want no response for all-members mention", snap)
	}
}

func TestHandleAllMembersMentionWithoutMetadataSkipsMessage(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, botOpenID: testBotOpenID}

	if err := b.handleMessage(context.Background(), feishuEvent("group", "text", `{"text":"前缀 @_all 现在呢"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	if processor.snapshot().called {
		t.Fatal("processor was called for all-members mention without metadata")
	}
	snap := sender.snapshot()
	if snap.called || len(snap.messages) != 0 || len(snap.replyCreates) != 0 || len(snap.reactionAdds) != 0 {
		t.Fatalf("sender = %#v, want no response for all-members mention without metadata", snap)
	}
}

func TestHandleGroupMessageWithoutBotMentionSkipsMessage(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, botOpenID: testBotOpenID}

	if err := b.handleMessage(context.Background(), feishuEvent("group", "text", `{"text":"hi"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	if processor.snapshot().called {
		t.Fatal("processor was called for group message without bot mention")
	}
	snap := sender.snapshot()
	if snap.called || len(snap.messages) != 0 || len(snap.replyCreates) != 0 || len(snap.reactionAdds) != 0 {
		t.Fatalf("sender = %#v, want no response for group message without bot mention", snap)
	}
}

func TestHandleUnsupportedMessageSendsNotice(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}

	if err := b.handleMessage(context.Background(), feishuEvent("p2p", "image", `{}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	senderSnap := waitForSentMessages(t, sender, 1)
	if processor.snapshot().called {
		t.Fatal("processor was called for unsupported message")
	}
	if !senderSnap.called || senderSnap.chatID != "oc_chat" || senderSnap.text != unsupportedMessageText {
		t.Fatalf("sender = %#v", senderSnap)
	}
	if len(sender.snapshot().reactionAdds) != 0 {
		t.Fatal("reaction was added for unsupported message")
	}
}

func TestHandleGroupUnsupportedMessageRepliesToOriginal(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, botOpenID: testBotOpenID}
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_bot_1", "bot", testBotOpenID, "", "LingoBridge"),
	}

	if err := b.handleMessage(context.Background(), feishuEvent("group", "image", `{}`, mentions)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	senderSnap := waitForReplyCreates(t, sender, 1)
	if processor.snapshot().called {
		t.Fatal("processor was called for unsupported message")
	}
	if len(senderSnap.replyCreates) != 1 || senderSnap.replyCreates[0].messageID != "om_message" || senderSnap.replyCreates[0].text != unsupportedMessageText {
		t.Fatalf("reply creates = %#v, want unsupported reply to om_message", senderSnap.replyCreates)
	}
	if len(senderSnap.messages) != 0 {
		t.Fatalf("messages = %#v, want no plain sends", senderSnap.messages)
	}
}

func TestHandleTextMessageUsesBridgeAndReplies(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{
		handler: processor,
		sender:  sender,
		account: store.Account{ID: "feishu:cli_test", Name: "admin-bot"},
	}

	if err := b.handleMessage(context.Background(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	processorSnap := waitForProcessorCalls(t, processor, 1)
	senderSnap := waitForSentMessages(t, sender, 1)
	if !processorSnap.called || processorSnap.userID != "feishu:ou_user" || processorSnap.text != "hi" {
		t.Fatalf("processor = %#v", processorSnap)
	}
	if processorSnap.platform != store.PlatformFeishu || processorSnap.accountID != "feishu:cli_test" || processorSnap.accountName != "admin-bot" {
		t.Fatalf("processor account scope = %#v, want feishu cli_test admin-bot", processorSnap)
	}
	if !senderSnap.called || senderSnap.chatID != "oc_chat" || senderSnap.text != "ok" {
		t.Fatalf("sender = %#v", senderSnap)
	}
	reactionSnap := waitForReactionDeletes(t, sender, 1)
	if len(reactionSnap.reactionAdds) != 1 || reactionSnap.reactionAdds[0].messageID != "om_message" || reactionSnap.reactionAdds[0].emojiType != feishuProcessingReactionEmoji {
		t.Fatalf("reaction adds = %#v, want Typing reaction on om_message", reactionSnap.reactionAdds)
	}
	if len(reactionSnap.reactionDeletes) != 1 || reactionSnap.reactionDeletes[0].messageID != "om_message" || reactionSnap.reactionDeletes[0].reactionID != "reaction-1" {
		t.Fatalf("reaction deletes = %#v, want reaction-1 delete on om_message", reactionSnap.reactionDeletes)
	}
}

func TestHandleGroupTextMessageRepliesToOriginal(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{
		handler:   processor,
		sender:    sender,
		botOpenID: testBotOpenID,
		account:   store.Account{ID: "feishu:cli_test", Name: "admin-bot"},
		tools:     []tooltypes.Tool{fakeCoreTool{}},
	}
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_bot_1", "bot", testBotOpenID, "", "LingoBridge"),
	}

	if err := b.handleMessage(context.Background(), feishuEvent("group", "text", `{"text":"@_bot_1 hi"}`, mentions)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	processorSnap := waitForProcessorCalls(t, processor, 1)
	senderSnap := waitForReplyCreates(t, sender, 1)
	if !processorSnap.called || processorSnap.userID != "feishu:group:oc_chat" || processorSnap.text != "hi" {
		t.Fatalf("processor = %#v", processorSnap)
	}
	if processorSnap.tools != 1 {
		t.Fatalf("processor tools = %d, want 1", processorSnap.tools)
	}
	if processorSnap.metadata["feishu.chat_id"] != "oc_chat" || processorSnap.metadata["feishu.message_id"] != "om_message" || processorSnap.metadata["feishu.sender_open_id"] != "ou_user" {
		t.Fatalf("processor metadata = %#v", processorSnap.metadata)
	}
	if !processorSnap.actorOK || processorSnap.actor.OpenID != "ou_user" {
		t.Fatalf("processor actor = %#v ok=%v, want sender open_id", processorSnap.actor, processorSnap.actorOK)
	}
	if !processorSnap.chatOK || processorSnap.chat.ChatID != "oc_chat" || processorSnap.chat.MessageID != "om_message" || !processorSnap.chat.IsGroup {
		t.Fatalf("processor chat = %#v ok=%v, want current group chat", processorSnap.chat, processorSnap.chatOK)
	}
	if !processorSnap.executionOK || processorSnap.execution.Platform != store.PlatformFeishu || processorSnap.execution.AccountID != "feishu:cli_test" || processorSnap.execution.UserKey != "feishu:group:oc_chat" {
		t.Fatalf("processor execution context = %#v ok=%v, want trusted runtime scope", processorSnap.execution, processorSnap.executionOK)
	}
	if len(senderSnap.replyCreates) != 1 || senderSnap.replyCreates[0].messageID != "om_message" || senderSnap.replyCreates[0].text != "ok" {
		t.Fatalf("reply creates = %#v, want ok reply to om_message", senderSnap.replyCreates)
	}
	if len(senderSnap.messages) != 0 {
		t.Fatalf("messages = %#v, want no plain sends", senderSnap.messages)
	}
}

func TestNewFeishuToolsRegistersEnabledChatHistory(t *testing.T) {
	cfg := feishutools.Config{ChatHistory: feishutools.ChatHistoryConfig{Enabled: true}}
	names := toolNames(newFeishuTools(&lark.Client{}, nil, "", cfg, nil, nil))
	if len(names) != 1 || names[0] != "feishu_chat_history_get" {
		t.Fatalf("tool names = %#v, want chat history", names)
	}
}

func TestNewFeishuToolsRegistersApprovalGatedDocumentWrites(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	cfg := feishutools.Config{
		Docs: feishutools.DocsToolsConfig{Enabled: true, AllowWrite: true},
	}
	withoutApproval := toolNames(newFeishuTools(&lark.Client{}, st, "feishu:cli_test", cfg, nil, nil))
	withoutApprovalNames := strings.Join(withoutApproval, ",")
	if strings.Contains(withoutApprovalNames, "feishu_docs_create") || strings.Contains(withoutApprovalNames, "feishu_docs_append") {
		t.Fatalf("tools without approval workflow = %#v, document writes must fail closed", withoutApproval)
	}
	withApproval := toolNames(newFeishuTools(&lark.Client{}, st, "feishu:cli_test", cfg, fakeToolApprovalRequester{}, fakeResourceAccessController{}))
	if got, want := strings.Join(withApproval, ","), "feishu_docs_request_access,feishu_docs_search,feishu_docs_read,feishu_docs_create,feishu_docs_append,feishu_docs_folder_create,feishu_docs_folder_list"; got != want {
		t.Fatalf("tools with approval workflow = %q, want %q", got, want)
	}
}

func TestFeishuResponderStreamsTextUpdatesOneMessage(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{sender: sender, chatID: "oc_chat"}

	stream, err := resp.StartTextStream(context.Background())
	if err != nil {
		t.Fatalf("StartTextStream returned error: %v", err)
	}
	if err := stream.Update(context.Background(), "hello"); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := stream.Update(context.Background(), "hello world"); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := stream.Finish(context.Background(), "hello world"); err != nil {
		t.Fatalf("Finish returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.streamCreates) != 1 || snap.streamCreates[0].chatID != "oc_chat" || snap.streamCreates[0].text != "hello" {
		t.Fatalf("stream creates = %#v, want one initial message", snap.streamCreates)
	}
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].messageID != "om_stream" || snap.streamUpdates[0].text != "hello world" {
		t.Fatalf("stream updates = %#v, want final update", snap.streamUpdates)
	}
	if len(snap.messages) != 0 {
		t.Fatalf("messages = %#v, want no separate SendText calls", snap.messages)
	}
}

func TestFeishuResponderStreamsGroupReplyThenUpdatesIt(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{sender: sender, chatID: "oc_chat", replyToMessageID: "om_original"}

	stream, err := resp.StartTextStream(context.Background())
	if err != nil {
		t.Fatalf("StartTextStream returned error: %v", err)
	}
	if err := stream.Update(context.Background(), "hello"); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := stream.Finish(context.Background(), "hello world"); err != nil {
		t.Fatalf("Finish returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.replyCreates) != 1 || snap.replyCreates[0].messageID != "om_original" || snap.replyCreates[0].text != "hello" {
		t.Fatalf("reply creates = %#v, want initial reply", snap.replyCreates)
	}
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].messageID != "om_reply" || snap.streamUpdates[0].text != "hello world" {
		t.Fatalf("stream updates = %#v, want final update on reply", snap.streamUpdates)
	}
	if len(snap.streamCreates) != 0 || len(snap.messages) != 0 {
		t.Fatalf("stream creates/messages = %#v/%#v, want none", snap.streamCreates, snap.messages)
	}
}

func TestFeishuResponderRendersMatchedMentionOnSend(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{
		sender: sender,
		chatID: "oc_chat",
		mentions: []feishuMention{
			{Name: "Alice", OpenID: "ou_alice"},
		},
	}

	if err := resp.Send(context.Background(), core.OutboundMessage{Text: "@Alice hello @Bob foo@Alice.com @AliceBob @Alice."}); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}

	snap := sender.snapshot()
	want := `<at user_id="ou_alice">Alice</at> hello @Bob foo@Alice.com @AliceBob <at user_id="ou_alice">Alice</at>.`
	if len(snap.messages) != 1 || snap.messages[0].text != want {
		t.Fatalf("messages = %#v, want %q", snap.messages, want)
	}
}

func TestFeishuResponderKeepsAmbiguousMentionOnSend(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{
		sender: sender,
		chatID: "oc_chat",
		mentions: []feishuMention{
			{Name: "Alice", OpenID: "ou_alice_one"},
			{Name: "Alice", OpenID: "ou_alice_two"},
		},
	}

	if err := resp.Send(context.Background(), core.OutboundMessage{Text: "@Alice hello"}); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.messages) != 1 || snap.messages[0].text != "@Alice hello" {
		t.Fatalf("messages = %#v, want raw text", snap.messages)
	}
}

func TestFeishuResponderKeepsMentionWithoutTargetIDOnSend(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{
		sender: sender,
		chatID: "oc_chat",
		mentions: []feishuMention{
			{Name: "Alice", OpenID: "ou_alice"},
			{Name: "Alice"},
		},
	}

	if err := resp.Send(context.Background(), core.OutboundMessage{Text: "@Alice hello"}); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.messages) != 1 || snap.messages[0].text != "@Alice hello" {
		t.Fatalf("messages = %#v, want raw text", snap.messages)
	}
}

func TestFeishuResponderStreamsRawPreviewAndRendersMentionOnFinish(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{
		sender: sender,
		chatID: "oc_chat",
		mentions: []feishuMention{
			{Name: "Alice", OpenID: "ou_alice"},
		},
	}

	stream, err := resp.StartTextStream(context.Background())
	if err != nil {
		t.Fatalf("StartTextStream returned error: %v", err)
	}
	if err := stream.Update(context.Background(), "@Alice thinking"); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := stream.Finish(context.Background(), "@Alice final"); err != nil {
		t.Fatalf("Finish returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.streamCreates) != 1 || snap.streamCreates[0].text != "@Alice thinking" {
		t.Fatalf("stream creates = %#v, want raw preview", snap.streamCreates)
	}
	want := `<at user_id="ou_alice">Alice</at> final`
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].text != want {
		t.Fatalf("stream updates = %#v, want rendered final %q", snap.streamUpdates, want)
	}
}

func TestFeishuResponderCompactNoticeCreatesMessageUpdatesSummaryAndMarksDone(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{sender: sender, chatID: "oc_chat", messageID: "om_compact_command"}
	notice := core.CompactNotice{
		ModelName:         "deepseek",
		Manual:            true,
		CompactedMessages: 2,
		RetainedMessages:  12,
	}

	handle, err := resp.StartCompactNotice(context.Background(), notice)
	if err != nil {
		t.Fatalf("StartCompactNotice returned error: %v", err)
	}
	if handle.MessageID != "om_stream" {
		t.Fatalf("compact notice handle = %#v, want om_stream", handle)
	}
	if err := resp.FinishCompactNotice(context.Background(), handle, notice); err != nil {
		t.Fatalf("FinishCompactNotice returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.streamCreates) != 1 || snap.streamCreates[0].chatID != "oc_chat" || snap.streamCreates[0].text != core.CompactStartText() {
		t.Fatalf("stream creates = %#v, want compact progress message", snap.streamCreates)
	}
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].messageID != "om_stream" || snap.streamUpdates[0].text != core.CompactSuccessText(notice) {
		t.Fatalf("stream updates = %#v, want compact success summary update", snap.streamUpdates)
	}
	if len(snap.reactionAdds) != 1 || snap.reactionAdds[0].messageID != "om_compact_command" || snap.reactionAdds[0].emojiType != feishuCompactDoneReactionEmoji {
		t.Fatalf("reaction adds = %#v, want DONE reaction on original /compact message", snap.reactionAdds)
	}
	if len(snap.messages) != 0 {
		t.Fatalf("messages = %#v, want no extra compact text message", snap.messages)
	}
}

func TestFeishuResponderCompactNoticeRepliesToOriginalInGroup(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{sender: sender, chatID: "oc_chat", messageID: "om_compact_command", replyToMessageID: "om_compact_command"}
	notice := core.CompactNotice{ModelName: "deepseek", Manual: true, CompactedMessages: 2, RetainedMessages: 12}

	handle, err := resp.StartCompactNotice(context.Background(), notice)
	if err != nil {
		t.Fatalf("StartCompactNotice returned error: %v", err)
	}
	if handle.MessageID != "om_reply" {
		t.Fatalf("compact notice handle = %#v, want om_reply", handle)
	}
	if err := resp.FinishCompactNotice(context.Background(), handle, notice); err != nil {
		t.Fatalf("FinishCompactNotice returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.replyCreates) != 1 || snap.replyCreates[0].messageID != "om_compact_command" || snap.replyCreates[0].text != core.CompactStartText() {
		t.Fatalf("reply creates = %#v, want compact progress reply", snap.replyCreates)
	}
	if len(snap.streamCreates) != 0 || len(snap.messages) != 0 {
		t.Fatalf("stream creates/messages = %#v/%#v, want none", snap.streamCreates, snap.messages)
	}
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].messageID != "om_reply" || snap.streamUpdates[0].text != core.CompactSuccessText(notice) {
		t.Fatalf("stream updates = %#v, want compact success update on reply", snap.streamUpdates)
	}
	if len(snap.reactionAdds) != 1 || snap.reactionAdds[0].messageID != "om_compact_command" || snap.reactionAdds[0].emojiType != feishuCompactDoneReactionEmoji {
		t.Fatalf("reaction adds = %#v, want DONE reaction on original /compact message", snap.reactionAdds)
	}
}

func TestFeishuResponderCompactNoticeDoesNotFallbackToText(t *testing.T) {
	sender := &fakeSender{addReactionErr: errors.New("reaction denied")}
	resp := feishuResponder{sender: sender, chatID: "oc_chat", messageID: "om_compact_command"}
	notice := core.CompactNotice{ModelName: "deepseek", CompactedMessages: 2, RetainedMessages: 12}

	if err := resp.FinishCompactNotice(context.Background(), core.CompactNoticeHandle{MessageID: "om_compact"}, notice); err == nil {
		t.Fatal("FinishCompactNotice returned nil error, want reaction error")
	}
	snap := sender.snapshot()
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].messageID != "om_compact" || snap.streamUpdates[0].text != core.CompactSuccessText(notice) {
		t.Fatalf("stream updates = %#v, want compact success summary update before reaction", snap.streamUpdates)
	}
	if len(snap.reactionAdds) != 1 || snap.reactionAdds[0].messageID != "om_compact_command" || snap.reactionAdds[0].emojiType != feishuCompactDoneReactionEmoji {
		t.Fatalf("reaction adds = %#v, want attempted DONE reaction on original /compact message", snap.reactionAdds)
	}
	if len(snap.messages) != 0 {
		t.Fatalf("messages = %#v, want no text fallback", snap.messages)
	}
}

func TestFeishuResponderCompactNoticeUpdateFailureDoesNotMarkDone(t *testing.T) {
	sender := &fakeSender{updateTextErr: errors.New("update denied")}
	resp := feishuResponder{sender: sender, chatID: "oc_chat"}
	notice := core.CompactNotice{ModelName: "deepseek", CompactedMessages: 2, RetainedMessages: 12}

	if err := resp.FinishCompactNotice(context.Background(), core.CompactNoticeHandle{MessageID: "om_compact"}, notice); err == nil {
		t.Fatal("FinishCompactNotice returned nil error, want update error")
	}
	snap := sender.snapshot()
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].messageID != "om_compact" || snap.streamUpdates[0].text != core.CompactSuccessText(notice) {
		t.Fatalf("stream updates = %#v, want attempted compact success summary update", snap.streamUpdates)
	}
	if len(snap.reactionAdds) != 0 {
		t.Fatalf("reaction adds = %#v, want no DONE reaction after update failure", snap.reactionAdds)
	}
	if len(snap.messages) != 0 {
		t.Fatalf("messages = %#v, want no text fallback", snap.messages)
	}
}

func TestFeishuResponderCompactNoticeWithoutOriginalMessageIDOnlyUpdatesSummary(t *testing.T) {
	sender := &fakeSender{}
	resp := feishuResponder{sender: sender, chatID: "oc_chat"}
	notice := core.CompactNotice{ModelName: "deepseek", CompactedMessages: 2, RetainedMessages: 12}

	if err := resp.FinishCompactNotice(context.Background(), core.CompactNoticeHandle{MessageID: "om_compact"}, notice); err != nil {
		t.Fatalf("FinishCompactNotice returned error: %v", err)
	}
	snap := sender.snapshot()
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].messageID != "om_compact" || snap.streamUpdates[0].text != core.CompactSuccessText(notice) {
		t.Fatalf("stream updates = %#v, want compact success summary update", snap.streamUpdates)
	}
	if len(snap.reactionAdds) != 0 {
		t.Fatalf("reaction adds = %#v, want no DONE reaction without original message id", snap.reactionAdds)
	}
}

func TestFeishuResponderCompactNoticeCreateFailureReturnsErr(t *testing.T) {
	sender := &fakeSender{createTextErr: errors.New("create denied")}
	resp := feishuResponder{sender: sender, chatID: "oc_chat"}

	if _, err := resp.StartCompactNotice(context.Background(), core.CompactNotice{ModelName: "deepseek"}); err == nil {
		t.Fatal("StartCompactNotice returned nil error, want create error")
	}
	if len(sender.snapshot().messages) != 0 {
		t.Fatalf("messages = %#v, want no text fallback", sender.snapshot().messages)
	}
}

func TestFeishuTextStreamCreateDoesNotCountAsEdit(t *testing.T) {
	clock := newFakeClock()
	stream := &feishuTextStream{
		sender: &fakeSender{},
		chatID: "oc_chat",
		now:    clock.now,
	}

	if err := stream.Update(context.Background(), "hello"); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if stream.editCount != 0 {
		t.Fatalf("editCount = %d, want 0 after create", stream.editCount)
	}
}

func TestFeishuTextStreamUsesDynamicPreviewIntervals(t *testing.T) {
	tests := []struct {
		name      string
		editCount int
		interval  time.Duration
	}{
		{name: "first previews", editCount: 0, interval: 300 * time.Millisecond},
		{name: "middle previews", editCount: 3, interval: 800 * time.Millisecond},
		{name: "late previews", editCount: 8, interval: 1500 * time.Millisecond},
		{name: "last previews", editCount: 14, interval: 2500 * time.Millisecond},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			sender := &fakeSender{}
			stream := &feishuTextStream{
				sender:       sender,
				chatID:       "oc_chat",
				messageID:    "om_stream",
				lastUpdateAt: clock.now(),
				lastSentText: "before",
				editCount:    tc.editCount,
				now:          clock.now,
			}

			clock.advance(tc.interval - time.Millisecond)
			if err := stream.Update(context.Background(), "too soon"); err != nil {
				t.Fatalf("Update before interval returned error: %v", err)
			}
			if got := len(sender.snapshot().streamUpdates); got != 0 {
				t.Fatalf("updates before interval = %d, want 0", got)
			}

			clock.advance(time.Millisecond)
			if err := stream.Update(context.Background(), "on time"); err != nil {
				t.Fatalf("Update at interval returned error: %v", err)
			}
			snap := sender.snapshot()
			if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].text != "on time" {
				t.Fatalf("updates at interval = %#v, want one update", snap.streamUpdates)
			}
			if stream.editCount != tc.editCount+1 {
				t.Fatalf("editCount = %d, want %d", stream.editCount, tc.editCount+1)
			}
		})
	}
}

func TestFeishuTextStreamStopsPreviewAtBudgetButFinishUpdates(t *testing.T) {
	clock := newFakeClock()
	sender := &fakeSender{}
	stream := &feishuTextStream{
		sender:       sender,
		chatID:       "oc_chat",
		messageID:    "om_stream",
		lastUpdateAt: clock.now(),
		lastSentText: "preview",
		editCount:    feishuMaxStreamPreviewEdits,
		now:          clock.now,
	}

	clock.advance(10 * time.Second)
	if err := stream.Update(context.Background(), "ignored preview"); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if got := len(sender.snapshot().streamUpdates); got != 0 {
		t.Fatalf("preview updates after budget = %d, want 0", got)
	}

	if err := stream.Finish(context.Background(), "final answer"); err != nil {
		t.Fatalf("Finish returned error: %v", err)
	}
	snap := sender.snapshot()
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].text != "final answer" {
		t.Fatalf("updates after Finish = %#v, want final update", snap.streamUpdates)
	}
	if len(snap.messages) != 0 {
		t.Fatalf("messages = %#v, want no fallback send", snap.messages)
	}
}

func TestFeishuResponderFallsBackToSendWhenEditLimitReached(t *testing.T) {
	sender := &fakeSender{updateTextErr: ErrFeishuMessageEditLimit}
	resp := feishuResponder{sender: sender, chatID: "oc_chat"}

	stream, err := resp.StartTextStream(context.Background())
	if err != nil {
		t.Fatalf("StartTextStream returned error: %v", err)
	}
	if err := stream.Update(context.Background(), "partial"); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := stream.Finish(context.Background(), "final answer"); err != nil {
		t.Fatalf("Finish returned error: %v", err)
	}

	snap := sender.snapshot()
	if len(snap.streamCreates) != 1 || snap.streamCreates[0].text != "partial" {
		t.Fatalf("stream creates = %#v, want partial create", snap.streamCreates)
	}
	if len(snap.streamUpdates) != 1 || snap.streamUpdates[0].text != "final answer" {
		t.Fatalf("stream updates = %#v, want attempted final update", snap.streamUpdates)
	}
	if len(snap.messages) != 1 || snap.messages[0].chatID != "oc_chat" || snap.messages[0].text != "final answer" {
		t.Fatalf("messages = %#v, want fallback final answer", snap.messages)
	}
}

func TestHandleHelpMessagePassesCommandToBridge(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}

	if err := b.handleMessage(context.Background(), feishuEvent("p2p", "text", `{"text":"/help"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	processorSnap := waitForProcessorCalls(t, processor, 1)
	if !processorSnap.called || processorSnap.userID != "feishu:ou_user" || processorSnap.commandText != "/help" || processorSnap.text != "/help" {
		t.Fatalf("processor = %#v", processorSnap)
	}
	if len(sender.snapshot().reactionAdds) != 0 {
		t.Fatal("reaction was added for slash command")
	}
}

func TestHandleGroupHelpMessageRepliesToOriginal(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, botOpenID: testBotOpenID}
	mentions := []*larkim.MentionEvent{
		feishuMentionWithName("@_bot_1", "bot", testBotOpenID, "", "LingoBridge"),
	}

	if err := b.handleMessage(context.Background(), feishuEvent("group", "text", `{"text":"@_bot_1 /help"}`, mentions)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	processorSnap := waitForProcessorCalls(t, processor, 1)
	senderSnap := waitForReplyCreates(t, sender, 1)
	if !processorSnap.called || processorSnap.userID != "feishu:group:oc_chat" || processorSnap.commandText != "/help" || processorSnap.text != "/help" {
		t.Fatalf("processor = %#v", processorSnap)
	}
	if len(senderSnap.replyCreates) != 1 || senderSnap.replyCreates[0].messageID != "om_message" || senderSnap.replyCreates[0].text != "ok" {
		t.Fatalf("reply creates = %#v, want slash reply to om_message", senderSnap.replyCreates)
	}
	if len(senderSnap.reactionAdds) != 0 {
		t.Fatal("reaction was added for slash command")
	}
	if len(senderSnap.messages) != 0 {
		t.Fatalf("messages = %#v, want no plain sends", senderSnap.messages)
	}
}

func TestHandleGroupHelpMessageWithoutBotMentionSkipsMessage(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, botOpenID: testBotOpenID}

	if err := b.handleMessage(context.Background(), feishuEvent("group", "text", `{"text":"/help"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	if processor.snapshot().called {
		t.Fatal("processor was called for group slash command without bot mention")
	}
	if snap := sender.snapshot(); snap.called || len(snap.messages) != 0 || len(snap.replyCreates) != 0 || len(snap.reactionAdds) != 0 {
		t.Fatalf("sender = %#v, want no response for group slash command without bot mention", snap)
	}
}

func TestHandleDuplicateMessageIDIgnored(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}
	event := feishuEventWithIDs("p2p", "text", `{"text":"hi"}`, nil, "om_same", "event_one")

	if err := b.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("first handleMessage returned error: %v", err)
	}
	if err := b.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("second handleMessage returned error: %v", err)
	}

	waitForProcessorCalls(t, processor, 1)
	waitForSentMessages(t, sender, 1)
	time.Sleep(50 * time.Millisecond)
	if got := processor.snapshot().calls; got != 1 {
		t.Fatalf("processor calls = %d, want one", got)
	}
	if got := len(sender.snapshot().messages); got != 1 {
		t.Fatalf("sent messages = %d, want one", got)
	}
	reactionSnap := waitForReactionDeletes(t, sender, 1)
	if len(reactionSnap.reactionAdds) != 1 || len(reactionSnap.reactionDeletes) != 1 {
		t.Fatalf("reaction adds/deletes = %#v/%#v, want one each", reactionSnap.reactionAdds, reactionSnap.reactionDeletes)
	}
}

func TestHandleDuplicateFallsBackToEventID(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}
	event := feishuEventWithIDs("p2p", "text", `{"text":"hi"}`, nil, "", "event_same")

	if err := b.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("first handleMessage returned error: %v", err)
	}
	if err := b.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("second handleMessage returned error: %v", err)
	}

	waitForProcessorCalls(t, processor, 1)
	time.Sleep(50 * time.Millisecond)
	if got := processor.snapshot().calls; got != 1 {
		t.Fatalf("processor calls = %d, want one", got)
	}
}

func TestHandleDifferentMessageIDsProcessed(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}

	if err := b.handleMessage(context.Background(), feishuEventWithIDs("p2p", "text", `{"text":"one"}`, nil, "om_one", "event_one")); err != nil {
		t.Fatalf("first handleMessage returned error: %v", err)
	}
	if err := b.handleMessage(context.Background(), feishuEventWithIDs("p2p", "text", `{"text":"two"}`, nil, "om_two", "event_two")); err != nil {
		t.Fatalf("second handleMessage returned error: %v", err)
	}

	waitForProcessorCalls(t, processor, 2)
	waitForSentMessages(t, sender, 2)
	if got := processor.snapshot().calls; got != 2 {
		t.Fatalf("processor calls = %d, want two", got)
	}
	if got := len(sender.snapshot().messages); got != 2 {
		t.Fatalf("sent messages = %d, want two", got)
	}
}

func TestHandleDuplicateUnsupportedMessageSendsOneNotice(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}
	event := feishuEventWithIDs("p2p", "image", `{}`, nil, "om_image", "event_image")

	if err := b.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("first handleMessage returned error: %v", err)
	}
	if err := b.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("second handleMessage returned error: %v", err)
	}

	waitForSentMessages(t, sender, 1)
	time.Sleep(50 * time.Millisecond)
	if processor.snapshot().called {
		t.Fatal("processor was called for unsupported message")
	}
	messages := sender.snapshot().messages
	if len(messages) != 1 || messages[0].text != unsupportedMessageText {
		t.Fatalf("messages = %#v, want one unsupported notice", messages)
	}
}

func TestHandleTextMessageSkipsReactionWithoutMessageID(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}

	if err := b.handleMessage(context.Background(), feishuEventWithIDs("p2p", "text", `{"text":"hi"}`, nil, "", "event_no_message_id")); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	waitForProcessorCalls(t, processor, 1)
	waitForSentMessages(t, sender, 1)
	if len(sender.snapshot().reactionAdds) != 0 {
		t.Fatal("reaction was added without message_id")
	}
}

func TestHandleTextMessageContinuesWhenAddReactionFails(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{addReactionErr: errors.New("reaction denied")}
	b := &bot{handler: processor, sender: sender}

	if err := b.handleMessage(context.Background(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	waitForProcessorCalls(t, processor, 1)
	waitForSentMessages(t, sender, 1)
	snap := sender.snapshot()
	if len(snap.reactionAdds) != 1 {
		t.Fatalf("reaction adds = %#v, want one attempted add", snap.reactionAdds)
	}
	if len(snap.reactionDeletes) != 0 {
		t.Fatalf("reaction deletes = %#v, want none after add failure", snap.reactionDeletes)
	}
}

func TestHandleTextMessageContinuesWhenDeleteReactionFails(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{deleteReactionErr: errors.New("delete denied")}
	b := &bot{handler: processor, sender: sender}

	if err := b.handleMessage(context.Background(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	waitForProcessorCalls(t, processor, 1)
	waitForSentMessages(t, sender, 1)
	reactionSnap := waitForReactionDeletes(t, sender, 1)
	if len(reactionSnap.reactionAdds) != 1 || len(reactionSnap.reactionDeletes) != 1 {
		t.Fatalf("reaction adds/deletes = %#v/%#v, want one each", reactionSnap.reactionAdds, reactionSnap.reactionDeletes)
	}
}

func TestHandleTextMessageDelaysReactionDeleteAfterReply(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, reactionDelay: 200 * time.Millisecond}

	if err := b.handleMessage(context.Background(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}

	waitForProcessorCalls(t, processor, 1)
	waitForSentMessages(t, sender, 1)
	waitForReactionAdds(t, sender, 1)
	if got := len(sender.snapshot().reactionDeletes); got != 0 {
		t.Fatalf("reaction deletes immediately after reply = %d, want none", got)
	}
	waitForReactionDeletes(t, sender, 1)
}

func TestHandleMessageReturnsBeforeProcessorFinishes(t *testing.T) {
	processor := &fakeProcessor{started: make(chan struct{}), release: make(chan struct{})}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender}
	done := make(chan error, 1)

	go func() {
		done <- b.handleMessage(context.Background(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleMessage returned error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("handleMessage did not return before processor finished")
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	if got := len(sender.snapshot().messages); got != 0 {
		t.Fatalf("sent messages before release = %d, want none", got)
	}
	close(processor.release)
	waitForSentMessages(t, sender, 1)
}

func TestConfigureP2PChatCreatedSendsCommandOutput(t *testing.T) {
	processor := &fakeProcessor{}
	sender := &fakeSender{}
	b := &bot{handler: processor, sender: sender, eventCommands: map[string][]string{}}

	d, registered, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), []feishu.EventConfig{
		{Name: "p2p_chat_create", Version: "1.0", Run: feishu.ShellRun{
			`printf 'hello %s' "$LINGOBRIDGE_FEISHU_CHAT_ID"`,
			`printf '%s' "$LINGOBRIDGE_COMMAND_HELP"`,
		}},
	})
	if err != nil {
		t.Fatalf("configureEventHandlers returned error: %v", err)
	}
	if d == nil {
		t.Fatal("configureEventHandlers returned nil dispatcher")
	}
	if got, want := strings.Join(registered, ", "), "im.message.receive_v1, p2p_chat_create"; got != want {
		t.Fatalf("registered events = %q, want %q", got, want)
	}

	_, err = d.Do(context.Background(), []byte(`{
		"type":"event_callback",
		"event":{
			"type":"p2p_chat_create",
			"app_id":"cli_xxx",
			"chat_id":"oc_chat",
			"tenant_key":"tenant_xxx"
		}
	}`))
	if err != nil {
		t.Fatalf("dispatcher.Do returned error: %v", err)
	}
	if processor.called {
		t.Fatal("processor was called for p2p_chat_create")
	}
	if len(sender.messages) != 2 {
		t.Fatalf("messages = %#v, want two messages", sender.messages)
	}
	if sender.messages[0].chatID != "oc_chat" || sender.messages[0].text != "hello oc_chat" {
		t.Fatalf("first message = %#v, want greeting", sender.messages[0])
	}
	if sender.messages[1].chatID != "oc_chat" || sender.messages[1].text != commands.HelpText(commands.DefaultPolicy()) {
		t.Fatalf("second message = %#v, want help", sender.messages[1])
	}
	if !strings.Contains(sender.messages[1].text, "/help") || !strings.Contains(sender.messages[1].text, "/model") {
		t.Fatalf("help message = %q, want command help", sender.messages[1].text)
	}
}

func TestFeishuAccountRuntimeShutdownWaitsForInFlightCustomEvent(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	sender := &blockingSendTextSender{
		fakeSender: &fakeSender{},
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	b := &bot{
		sender: sender,
		eventCommands: map[string][]string{
			"p2p_chat_create": {"printf 'hello'"},
		},
		runCtx: runtimeCtx,
	}
	eventDone := make(chan error, 1)
	go func() {
		eventDone <- b.handleCustomizedEvent(context.Background(), "p2p_chat_create", &larkevent.EventReq{
			Body: []byte(`{"event":{"chat_id":"oc_chat"}}`),
		})
	}()
	select {
	case <-sender.started:
	case <-time.After(time.Second):
		t.Fatal("custom event did not reach its outbound send")
	}

	shutdownDone := make(chan struct{})
	go func() {
		_, _ = shutdownFeishuAccountRuntime(
			cancelRuntime,
			nil,
			nil,
			b,
			nil,
			nil,
			nil,
			nil,
		)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		close(sender.release)
		t.Fatal("runtime shutdown completed while an admitted custom event was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(sender.release)
	select {
	case err := <-eventDone:
		if err != nil {
			t.Fatalf("handleCustomizedEvent returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("custom event did not finish after release")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("runtime shutdown did not finish after the custom event returned")
	}
}

func TestConfigureBotP2PChatEnteredV2SendsCommandOutput(t *testing.T) {
	sender := &fakeSender{}
	b := &bot{handler: &fakeProcessor{}, sender: sender, eventCommands: map[string][]string{}}

	d, registered, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), []feishu.EventConfig{
		{Name: "im.chat.access_event.bot_p2p_chat_entered_v1", Version: "2.0", Run: feishu.ShellRun{
			`printf 'entered %s %s' "$LINGOBRIDGE_FEISHU_CHAT_ID" "$LINGOBRIDGE_FEISHU_OPERATOR_OPEN_ID"`,
		}},
	})
	if err != nil {
		t.Fatalf("configureEventHandlers returned error: %v", err)
	}
	if got, want := strings.Join(registered, ", "), "im.message.receive_v1, im.chat.access_event.bot_p2p_chat_entered_v1"; got != want {
		t.Fatalf("registered events = %q, want %q", got, want)
	}

	_, err = d.Do(context.Background(), []byte(`{
		"schema":"2.0",
		"header":{
			"event_type":"im.chat.access_event.bot_p2p_chat_entered_v1",
			"tenant_key":"tenant_xxx"
		},
		"event":{
			"chat_id":"oc_chat",
			"operator_id":{"open_id":"ou_operator","user_id":"user_operator"}
		}
	}`))
	if err != nil {
		t.Fatalf("dispatcher.Do returned error: %v", err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("messages = %#v, want one message", sender.messages)
	}
	if sender.messages[0].chatID != "oc_chat" || sender.messages[0].text != "entered oc_chat ou_operator" {
		t.Fatalf("message = %#v, want v2 greeting", sender.messages[0])
	}
}

func TestConfigureEventHandlersReportsBuiltInMessageEvent(t *testing.T) {
	b := &bot{eventCommands: map[string][]string{}}
	d, registered, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), nil)
	if err != nil {
		t.Fatalf("configureEventHandlers returned error: %v", err)
	}
	if d == nil {
		t.Fatal("configureEventHandlers returned nil dispatcher")
	}
	if got, want := strings.Join(registered, ", "), "im.message.receive_v1"; got != want {
		t.Fatalf("registered events = %q, want %q", got, want)
	}
}

func TestConfigureEventHandlersRejectsBuiltInMessageEvent(t *testing.T) {
	b := &bot{eventCommands: map[string][]string{}}
	_, _, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), []feishu.EventConfig{
		{Name: "im.message.receive_v1", Version: "2.0", Run: feishu.ShellRun{"echo nope"}},
	})
	if err == nil || !strings.Contains(err.Error(), "built in") {
		t.Fatalf("configureEventHandlers error = %v, want built in event error", err)
	}
}

func TestConfigureEventHandlersRejectsMissingVersion(t *testing.T) {
	b := &bot{eventCommands: map[string][]string{}}
	_, _, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), []feishu.EventConfig{
		{Name: "unknown", Run: feishu.ShellRun{"echo nope"}},
	})
	if err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatalf("configureEventHandlers error = %v, want missing version error", err)
	}
}

func TestConfigureEventHandlersRejectsUnsupportedVersion(t *testing.T) {
	b := &bot{eventCommands: map[string][]string{}}
	_, _, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), []feishu.EventConfig{
		{Name: "p2p_chat_create", Version: "3.0", Run: feishu.ShellRun{"echo nope"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("configureEventHandlers error = %v, want unsupported version error", err)
	}
}

func TestConfigureEventHandlersRejectsUnsupportedV2Event(t *testing.T) {
	b := &bot{eventCommands: map[string][]string{}}
	_, _, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), []feishu.EventConfig{
		{Name: "unknown", Version: "2.0", Run: feishu.ShellRun{"echo nope"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported feishu v2 event") {
		t.Fatalf("configureEventHandlers error = %v, want unsupported v2 event error", err)
	}
}

func TestRunFeishuEventCommandsSkipsStdoutWithoutChatID(t *testing.T) {
	sender := &fakeSender{}
	env := map[string]string{
		"LINGOBRIDGE_PLATFORM":   store.PlatformFeishu,
		"LINGOBRIDGE_EVENT_NAME": "event_without_chat",
	}
	if err := runFeishuEventCommands(context.Background(), sender, "event_without_chat", "", []string{"printf 'hello'"}, env); err != nil {
		t.Fatalf("runFeishuEventCommands returned error: %v", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("messages = %#v, want none", sender.messages)
	}
}

func TestFetchBotOpenID(t *testing.T) {
	var sawBotInfo bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/bot/v3/info":
			if r.Method != http.MethodGet {
				t.Fatalf("method = %s, want GET", r.Method)
			}
			sawBotInfo = true
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"bot":  map[string]any{"open_id": testBotOpenID},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	openID, err := fetchBotOpenID(context.Background(), client)
	if err != nil {
		t.Fatalf("fetchBotOpenID returned error: %v", err)
	}
	if !sawBotInfo || openID != testBotOpenID {
		t.Fatalf("openID = %q sawBotInfo=%v", openID, sawBotInfo)
	}
}

func TestFetchBotOpenIDRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    map[string]any
		wantErr string
	}{
		{
			name:    "http status",
			status:  http.StatusInternalServerError,
			body:    map[string]any{"code": 0, "msg": "ok"},
			wantErr: "status=500",
		},
		{
			name:    "nonzero code",
			status:  http.StatusOK,
			body:    map[string]any{"code": 999, "msg": "nope"},
			wantErr: "code=999",
		},
		{
			name:    "missing open id",
			status:  http.StatusOK,
			body:    map[string]any{"code": 0, "msg": "ok", "bot": map[string]any{}},
			wantErr: "missing bot.open_id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
					writeJSON(t, w, map[string]any{
						"code":                0,
						"msg":                 "ok",
						"tenant_access_token": "tenant-token",
						"expire":              7200,
					})
				case "/open-apis/bot/v3/info":
					w.WriteHeader(tc.status)
					writeJSON(t, w, tc.body)
				default:
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
			}))
			defer server.Close()

			client := lark.NewClient("cli_xxx", "secret",
				lark.WithOpenBaseUrl(server.URL),
				lark.WithOAuthBaseUrl(server.URL),
				lark.WithHttpClient(server.Client()),
			)
			_, err := fetchBotOpenID(context.Background(), client)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("fetchBotOpenID error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPlatformRunFailsWhenBotOpenIDMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/bot/v3/info":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"bot":  map[string]any{},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := store.Account{
		ID:              "feishu:cli_xxx",
		Name:            "fsbot",
		Platform:        store.PlatformFeishu,
		CredentialsJSON: `{}`,
	}

	err := NewPlatform(nil, acc, feishu.Config{
		Accounts: map[string]feishu.AccountConfig{
			"fsbot": {AppID: "cli_xxx", AppSecret: "secret", BaseURL: server.URL},
		},
	}, logging.Info).Run(context.Background(), &fakeProcessor{})
	if err == nil || !strings.Contains(err.Error(), "resolve feishu bot identity") || !strings.Contains(err.Error(), "missing bot.open_id") {
		t.Fatalf("Run error = %v, want bot identity error", err)
	}
}

func TestPlatformRunRequiresAccountCredentials(t *testing.T) {
	acc := store.Account{
		ID:              "feishu:cli_xxx",
		Name:            "fsbot",
		Platform:        store.PlatformFeishu,
		CredentialsJSON: `{}`,
	}

	err := NewPlatform(nil, acc, feishu.Config{
		Accounts: map[string]feishu.AccountConfig{
			"fsbot": {},
		},
	}, logging.Info).Run(context.Background(), &fakeProcessor{})
	if err == nil || !strings.Contains(err.Error(), "app_id is required") {
		t.Fatalf("Run error = %v, want missing credentials error", err)
	}
}

func TestPlatformRunRequiresConfiguredAccount(t *testing.T) {
	acc := store.Account{
		ID:              "feishu:cli_xxx",
		Name:            "fsbot",
		Platform:        store.PlatformFeishu,
		CredentialsJSON: `{}`,
	}

	err := NewPlatform(nil, acc, feishu.Config{}, logging.Info).Run(context.Background(), &fakeProcessor{})
	if err == nil || !strings.Contains(err.Error(), "platforms.feishu.accounts.fsbot is required") {
		t.Fatalf("Run error = %v, want missing account config error", err)
	}
}

func TestPlatformRunRequiresStoreForDocumentResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/bot/v3/info":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"bot":  map[string]any{"open_id": "ou_bot"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := store.Account{ID: "feishu:cli_xxx", Name: "fsbot", Platform: store.PlatformFeishu}
	err := NewPlatform(nil, acc, feishu.Config{
		Accounts: map[string]feishu.AccountConfig{
			"fsbot": {AppID: "cli_xxx", AppSecret: "secret", BaseURL: server.URL},
		},
		Tools: feishutools.Config{
			Docs: feishutools.DocsToolsConfig{Enabled: true, AllowWrite: true},
		},
	}, logging.Info).Run(context.Background(), &fakeProcessor{})
	if err == nil || !strings.Contains(err.Error(), "runtime lease requires a Feishu store") {
		t.Fatalf("Run error = %v, want missing Feishu runtime store", err)
	}
}

func TestPlatformRunHeldAccountLeaseSkipsStartupRecovery(t *testing.T) {
	activeStore, recoveringStore := openSharedFeishuApprovalTestStores(t)
	now := time.Now().UTC()
	if _, err := activeStore.AcquireFeishuAccountRuntimeLease("feishu:cli_xxx", "runtime_active", now, time.Minute); err != nil {
		t.Fatalf("acquire active runtime lease: %v", err)
	}
	t.Cleanup(func() {
		if err := activeStore.ReleaseFeishuAccountRuntimeLease("feishu:cli_xxx", "runtime_active"); err != nil {
			t.Errorf("release active runtime lease: %v", err)
		}
	})
	activeApproval, err := activeStore.CreateToolApproval(store.ToolApproval{
		AccountID:       "feishu:cli_xxx",
		ToolName:        "feishu_docs_create",
		ActionKey:       "create",
		ResourceType:    "folder",
		ResourceToken:   "fld_token",
		SupportsAll:     true,
		ActorOpenID:     "ou_requester",
		ActorUserID:     "u_requester",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		Payload:         `{"title":"Quarterly plan"}`,
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval returned error: %v", err)
	}
	if err := activeStore.SetToolApprovalCardMessageID(activeApproval.ID, activeApproval.AccountID, "om_card", now); err != nil {
		t.Fatalf("SetToolApprovalCardMessageID returned error: %v", err)
	}
	if _, err := activeStore.DecideToolApproval(
		activeApproval.ID,
		"feishu:cli_xxx",
		store.ToolApprovalDecisionApprove,
		store.ToolApprovalMatch{
			ActorOpenID:   "ou_requester",
			ActorUserID:   "u_requester",
			ChatID:        "oc_chat",
			CardMessageID: "om_card",
		},
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("DecideToolApproval returned error: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/bot/v3/info":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"bot":  map[string]any{"open_id": "ou_bot"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	acc := store.Account{ID: "feishu:cli_xxx", Name: "fsbot", Platform: store.PlatformFeishu}
	err = NewPlatform(recoveringStore, acc, feishu.Config{
		Accounts: map[string]feishu.AccountConfig{
			"fsbot": {AppID: "cli_xxx", AppSecret: "secret", BaseURL: server.URL},
		},
		Tools: feishutools.Config{Docs: feishutools.DocsToolsConfig{Enabled: true, AllowWrite: true}},
	}, logging.Info).Run(t.Context(), &fakeProcessor{})
	if !errors.Is(err, store.ErrFeishuAccountRuntimeLeaseHeld) {
		t.Fatalf("Run error = %v, want ErrFeishuAccountRuntimeLeaseHeld", err)
	}
	approval, loadErr := activeStore.GetToolApproval(activeApproval.ID, "feishu:cli_xxx")
	if loadErr != nil || approval.State != store.ToolApprovalStateExecuting {
		t.Fatalf("active approval after rejected second runtime = %#v err=%v, want executing", approval, loadErr)
	}
}
func TestFeishuSDKLogLevel(t *testing.T) {
	tests := []struct {
		level logging.Level
		want  larkcore.LogLevel
	}{
		{level: logging.All, want: larkcore.LogLevelDebug},
		{level: logging.Debug, want: larkcore.LogLevelInfo},
		{level: logging.Info, want: larkcore.LogLevelInfo},
		{level: logging.Warn, want: larkcore.LogLevelWarn},
		{level: logging.Error, want: larkcore.LogLevelError},
	}

	for _, tc := range tests {
		if got := feishuSDKLogLevel(tc.level); got != tc.want {
			t.Fatalf("feishuSDKLogLevel(%v) = %v, want %v", tc.level, got, tc.want)
		}
	}
}

func TestSDKLevelLoggerFiltersBeforeSharedLogger(t *testing.T) {
	next := &fakeSDKLogger{}
	logger := newSDKLevelLogger(larkcore.LogLevelInfo, next)

	logger.Debug(context.Background(), "hidden")
	logger.Info(context.Background(), "visible")

	if next.debugs != 0 || next.infos != 1 {
		t.Fatalf("info sdk logger counts: debug=%d info=%d, want debug=0 info=1", next.debugs, next.infos)
	}

	next = &fakeSDKLogger{}
	logger = newSDKLevelLogger(larkcore.LogLevelDebug, next)
	logger.Debug(context.Background(), "visible")
	logger.Debug(context.Background(), "receive message, payload: {sensitive}")
	logger.Debug(context.Background(), "event request: header:map[],body:{sensitive}")
	logger.Debug(context.Background(), "card request: header:map[],body:{sensitive}")

	if next.debugs != 1 {
		t.Fatalf("debug sdk logger debug count = %d, want one non-payload message", next.debugs)
	}
}

func TestRunClientClosesOnContextCancel(t *testing.T) {
	client := &blockingClient{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runClient(ctx, client, nil)
	}()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runClient returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runClient did not return after context cancel")
	}
	select {
	case <-client.closed:
	default:
		t.Fatal("client.Close was not called")
	}
}

type blockingClient struct {
	closed chan struct{}
}

func (b *blockingClient) Start(ctx context.Context) error {
	<-b.closed
	return nil
}

func (b *blockingClient) Close() {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
}

func feishuEvent(chatType, messageType, content string, mentions []*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
	return feishuEventWithIDs(chatType, messageType, content, mentions, "om_message", "event_message")
}

func feishuMentionEvent(key, mentionedType, openID string) *larkim.MentionEvent {
	return feishuMentionWithName(key, mentionedType, openID, "", "")
}

func feishuMentionWithName(key, mentionedType, openID, userID, name string) *larkim.MentionEvent {
	builder := larkim.NewMentionEventBuilder().
		Key(key).
		MentionedType(mentionedType)
	if name != "" {
		builder.Name(name)
	}
	if openID != "" || userID != "" {
		builder.Id(larkim.NewUserIdBuilder().OpenId(openID).UserId(userID).Build())
	}
	return builder.Build()
}

func assertMentions(t *testing.T, got []feishuMention, want ...feishuMention) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("mentions = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mentions[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func feishuEventWithIDs(chatType, messageType, content string, mentions []*larkim.MentionEvent, messageID, eventID string) *larkim.P2MessageReceiveV1 {
	return feishuEventWithSenderAndIDs(chatType, messageType, content, mentions, "ou_user", messageID, eventID)
}

func feishuEventWithSender(chatType, messageType, content string, mentions []*larkim.MentionEvent, senderOpenID string) *larkim.P2MessageReceiveV1 {
	return feishuEventWithSenderAndIDs(chatType, messageType, content, mentions, senderOpenID, "om_message", "event_message")
}

func feishuEventWithSenderAndIDs(chatType, messageType, content string, mentions []*larkim.MentionEvent, senderOpenID, messageID, eventID string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{EventID: eventID},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: larkim.NewEventSenderBuilder().
				SenderId(larkim.NewUserIdBuilder().OpenId(senderOpenID).Build()).
				SenderType("user").
				Build(),
			Message: larkim.NewEventMessageBuilder().
				ChatId("oc_chat").
				ChatType(chatType).
				MessageId(messageID).
				MessageType(messageType).
				Content(content).
				Mentions(mentions).
				Build(),
		},
	}
}
