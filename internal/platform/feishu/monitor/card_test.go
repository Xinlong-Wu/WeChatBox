package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

func TestCardServiceRoutesRegisteredActionKind(t *testing.T) {
	service, err := newCardService(&fakeApprovalSender{})
	if err != nil {
		t.Fatalf("newCardService returned error: %v", err)
	}
	called := false
	if err := service.RegisterAction("test_kind", func(_ context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		called = cardActionKind(event) == "test_kind"
		return cardToast("success", "handled"), nil
	}); err != nil {
		t.Fatalf("RegisterAction returned error: %v", err)
	}
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{"kind": "test_kind"}},
	}}
	response, err := service.HandleAction(t.Context(), event)
	if err != nil {
		t.Fatalf("HandleAction returned error: %v", err)
	}
	if !called || response == nil || response.Toast == nil || response.Toast.Content != "handled" {
		t.Fatalf("called/response = %v/%#v, want registered handler response", called, response)
	}
}

func TestCardServiceRejectsDuplicateAndUnknownActionKinds(t *testing.T) {
	service, err := newCardService(&fakeApprovalSender{})
	if err != nil {
		t.Fatalf("newCardService returned error: %v", err)
	}
	handler := func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		return nil, nil
	}
	if err := service.RegisterAction("test_kind", handler); err != nil {
		t.Fatalf("RegisterAction returned error: %v", err)
	}
	if err := service.RegisterAction("test_kind", handler); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate RegisterAction error = %v", err)
	}
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{"kind": "unknown_kind"}},
	}}
	response, err := service.HandleAction(t.Context(), event)
	if err != nil {
		t.Fatalf("HandleAction returned error: %v", err)
	}
	if response == nil || response.Toast == nil || response.Toast.Type != "error" {
		t.Fatalf("unknown action response = %#v, want error toast", response)
	}
}

func TestCardServiceSendsAndUpdatesConcreteCard(t *testing.T) {
	transport := &fakeApprovalSender{}
	service, err := newCardService(transport)
	if err != nil {
		t.Fatalf("newCardService returned error: %v", err)
	}
	card := statusCard{title: "完成", template: "green", message: "已完成"}
	messageID, err := service.Send(t.Context(), "oc_chat", card)
	if err != nil || messageID != "om_card" {
		t.Fatalf("Send returned id=%q err=%v", messageID, err)
	}
	if err := service.UpdateByMessageID(t.Context(), "om_card", card); err != nil {
		t.Fatalf("UpdateByMessageID returned error: %v", err)
	}
	if err := service.UpdateByCallbackToken(t.Context(), "c_callback", card); err != nil {
		t.Fatalf("UpdateByCallbackToken returned error: %v", err)
	}
	cards, updates, _ := transport.snapshot()
	if len(cards) != 1 || cards[0].chatID != "oc_chat" || !strings.Contains(cards[0].text, `"schema":"2.0"`) {
		t.Fatalf("sent cards = %#v", cards)
	}
	if len(updates) != 2 || updates[0].messageID != "om_card" || updates[1].messageID != "c_callback" {
		t.Fatalf("card updates = %#v", updates)
	}
}
