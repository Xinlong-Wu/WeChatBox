package core

import (
	"context"
	"strings"

	"lingobridge/internal/config"
	"lingobridge/internal/llm"
)

func defaultLLMFactory(model config.ResolvedModel) llm.Client {
	return llm.NewClient(llm.Config{
		Provider: model.Provider,
		BaseURL:  model.BaseURL,
		APIKey:   model.APIKey,
		Model:    model.ID,
		Endpoint: model.Endpoint,
		Compact: llm.CompactConfig{
			Mode:          string(model.Compact.Mode),
			ContextWindow: model.ContextWindow,
			Threshold:     model.Compact.Threshold,
			Instructions:  model.Compact.Instructions,
		},
	})
}

func (b *Bot) llmForSession(userID, sessionID string) (config.ResolvedModel, llm.Client, error) {
	modelName, err := b.Sessions.CurrentModel(userID, sessionID)
	if err != nil {
		return config.ResolvedModel{}, nil, err
	}
	return b.clientForModelName(modelName)
}

// llmForMessage resolves the LLM client for a message, preferring an explicit
// model override when it names a defined profile, otherwise falling back to the
// session's stored preference or the default model.
func (b *Bot) llmForMessage(ctx context.Context, msg InboundMessage, sessionID string) (config.ResolvedModel, llm.Client, error) {
	if name := strings.TrimSpace(msg.Model); name != "" {
		if b.LLMConfig.HasModel(name) {
			return b.clientForModelName(name)
		}
		coreLog.Warn(ctx, "requested model %q is not defined; falling back to session/default model", name)
	}
	return b.llmForSession(msg.UserKey, sessionID)
}

func (b *Bot) clientForModelName(modelName string) (config.ResolvedModel, llm.Client, error) {
	model, err := b.LLMConfig.ResolveModel(modelName)
	if err != nil {
		model, err = b.LLMConfig.ResolveModel(b.LLMConfig.DefaultModel)
		if err != nil {
			return config.ResolvedModel{}, nil, err
		}
	}
	newLLM := b.NewLLM
	if newLLM == nil {
		newLLM = defaultLLMFactory
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.LLMClients == nil {
		b.LLMClients = map[string]llm.Client{}
	}
	client, ok := b.LLMClients[model.Name]
	if !ok {
		client = newLLM(model)
		b.LLMClients[model.Name] = client
	}
	return model, client, nil
}
