package monitor

import (
	"context"
	"fmt"
	"strings"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"lingobridge/internal/platform/feishu"
)

const (
	feishuEventVersionV1 = "1.0"
	feishuEventVersionV2 = "2.0"

	feishuMessageReceiveEvent      = "im.message.receive_v1"
	feishuBotP2PChatEnteredV2Event = "im.chat.access_event.bot_p2p_chat_entered_v1"
	feishuCardActionEvent          = "card.action.trigger"
)

type feishuV2EventRegistrar func(*bot, *dispatcher.EventDispatcher) *dispatcher.EventDispatcher

var builtInFeishuV2EventRegistrars = map[string]feishuV2EventRegistrar{
	feishuMessageReceiveEvent: func(b *bot, d *dispatcher.EventDispatcher) *dispatcher.EventDispatcher {
		return d.OnP2MessageReceiveV1(b.handleMessage)
	},
}

var builtInFeishuV2EventNames = []string{
	feishuMessageReceiveEvent,
}

var configuredFeishuV2EventRegistrars = map[string]feishuV2EventRegistrar{
	feishuBotP2PChatEnteredV2Event: func(b *bot, d *dispatcher.EventDispatcher) *dispatcher.EventDispatcher {
		return d.OnP2ChatAccessEventBotP2pChatEnteredV1(b.handleBotP2PChatEntered)
	},
}

func (b *bot) configureEventHandlers(d *dispatcher.EventDispatcher, events []feishu.EventConfig) (*dispatcher.EventDispatcher, []string, error) {
	if b.eventCommands == nil {
		b.eventCommands = map[string][]string{}
	}
	registered := []string{}
	registeredHandlers := map[string]bool{}
	for _, name := range builtInFeishuV2EventNames {
		register := builtInFeishuV2EventRegistrars[name]
		d = register(b, d)
		registeredHandlers[feishuEventVersionV2+":"+name] = true
		registered = append(registered, name)
	}
	if b.cards != nil {
		d = d.OnP2CardActionTrigger(b.cards.HandleAction)
		registeredHandlers[feishuEventVersionV2+":"+feishuCardActionEvent] = true
		registered = append(registered, feishuCardActionEvent)
	}
	for i, event := range events {
		name := strings.TrimSpace(event.Name)
		version := strings.TrimSpace(event.Version)
		run := feishu.ShellRun(event.Run)
		if name == "" {
			return nil, nil, fmt.Errorf("platforms.feishu.events[%d].name is required", i)
		}
		if name == feishuMessageReceiveEvent || name == feishuCardActionEvent {
			return nil, nil, fmt.Errorf("platforms.feishu.events[%d].name %q is built in and cannot be configured", i, name)
		}
		if version == "" {
			return nil, nil, fmt.Errorf("platforms.feishu.events[%d].version is required", i)
		}
		if version != feishuEventVersionV1 && version != feishuEventVersionV2 {
			return nil, nil, fmt.Errorf("platforms.feishu.events[%d].version %q is unsupported; want \"1.0\" or \"2.0\"", i, version)
		}
		if len(run) == 0 {
			return nil, nil, fmt.Errorf("platforms.feishu.events[%d].run is required", i)
		}

		switch version {
		case feishuEventVersionV1:
			b.eventCommands[name] = append(b.eventCommands[name], run...)
			handlerKey := version + ":" + name
			if !registeredHandlers[handlerKey] {
				eventName := name
				d = d.OnCustomizedEvent(eventName, func(ctx context.Context, event *larkevent.EventReq) error {
					return b.handleCustomizedEvent(ctx, eventName, event)
				})
				registeredHandlers[handlerKey] = true
				registered = append(registered, name)
			}
		case feishuEventVersionV2:
			register, ok := configuredFeishuV2EventRegistrars[name]
			if !ok {
				return nil, nil, fmt.Errorf("unsupported feishu v2 event %q", name)
			}
			b.eventCommands[name] = append(b.eventCommands[name], run...)
			handlerKey := version + ":" + name
			if !registeredHandlers[handlerKey] {
				d = register(b, d)
				registeredHandlers[handlerKey] = true
				registered = append(registered, name)
			}
		}
	}
	return d, registered, nil
}

func (b *bot) handleCustomizedEvent(ctx context.Context, eventName string, event *larkevent.EventReq) error {
	env, chatID := customizedFeishuEventEnv(eventName, event)
	return runFeishuEventCommands(ctx, b.sender, eventName, chatID, b.eventCommands[eventName], env)
}

func (b *bot) handleBotP2PChatEntered(ctx context.Context, event *larkim.P2ChatAccessEventBotP2pChatEnteredV1) error {
	env, chatID := botP2PChatEnteredEnv(event)
	return runFeishuEventCommands(ctx, b.sender, feishuBotP2PChatEnteredV2Event, chatID, b.eventCommands[feishuBotP2PChatEnteredV2Event], env)
}
