package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// Card is one concrete Feishu Card V2 instance.
type Card interface {
	JSON() (string, error)
}

// CardActionHandler handles one registered card callback kind.
type CardActionHandler func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)

// CardService provides the single entry point for card rendering, transport,
// message/callback-token updates, and action routing.
type CardService interface {
	Render(Card) (string, error)
	Send(context.Context, string, Card) (string, error)
	UpdateByMessageID(context.Context, string, Card) error
	UpdateByCallbackToken(context.Context, string, Card) error
	RegisterAction(string, CardActionHandler) error
	HandleAction(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)
}

type cardTransport interface {
	CreateCard(ctx context.Context, chatID, cardJSON string) (string, error)
	UpdateCard(ctx context.Context, messageID, cardJSON string) error
	UpdateCardAfterInteraction(ctx context.Context, callbackToken, cardJSON string) error
}

type feishuCardService struct {
	transport cardTransport

	mu       sync.RWMutex
	handlers map[string]CardActionHandler
}

func newCardService(transport cardTransport) (*feishuCardService, error) {
	if transport == nil {
		return nil, fmt.Errorf("feishu card transport is required")
	}
	return &feishuCardService{
		transport: transport,
		handlers:  map[string]CardActionHandler{},
	}, nil
}

func (s *feishuCardService) Render(card Card) (string, error) {
	if card == nil {
		return "", fmt.Errorf("feishu card is required")
	}
	cardJSON, err := card.JSON()
	if err != nil {
		return "", fmt.Errorf("render feishu card: %w", err)
	}
	cardJSON, _, err = parseInteractiveCardJSON(cardJSON)
	if err != nil {
		return "", err
	}
	return cardJSON, nil
}

func (s *feishuCardService) Send(ctx context.Context, chatID string, card Card) (string, error) {
	cardJSON, err := s.Render(card)
	if err != nil {
		return "", err
	}
	return s.transport.CreateCard(ctx, strings.TrimSpace(chatID), cardJSON)
}

func (s *feishuCardService) UpdateByMessageID(ctx context.Context, messageID string, card Card) error {
	cardJSON, err := s.Render(card)
	if err != nil {
		return err
	}
	return s.transport.UpdateCard(ctx, strings.TrimSpace(messageID), cardJSON)
}

func (s *feishuCardService) UpdateByCallbackToken(ctx context.Context, callbackToken string, card Card) error {
	cardJSON, err := s.Render(card)
	if err != nil {
		return err
	}
	return s.transport.UpdateCardAfterInteraction(ctx, strings.TrimSpace(callbackToken), cardJSON)
}

func (s *feishuCardService) RegisterAction(kind string, handler CardActionHandler) error {
	kind = strings.TrimSpace(kind)
	if kind == "" || handler == nil {
		return fmt.Errorf("feishu card action kind and handler are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.handlers[kind]; exists {
		return fmt.Errorf("duplicate feishu card action kind %q", kind)
	}
	s.handlers[kind] = handler
	return nil
}

func (s *feishuCardService) HandleAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	kind := cardActionKind(event)
	if kind == "" {
		feishuLog.Warn(ctx, "ignored feishu card callback without action kind")
		return cardToast("error", "无法识别该卡片操作，请重新发起。"), nil
	}
	s.mu.RLock()
	handler := s.handlers[kind]
	s.mu.RUnlock()
	if handler == nil {
		feishuLog.Warn(ctx, "ignored unregistered feishu card callback kind=%s", kind)
		return cardToast("error", "该卡片操作当前不可用，请重新发起。"), nil
	}
	return handler(ctx, event)
}

func cardActionKind(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return ""
	}
	value, _ := event.Event.Action.Value["kind"].(string)
	return strings.TrimSpace(value)
}

func cardToast(toastType, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: toastType, Content: content},
	}
}
