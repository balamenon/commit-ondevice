package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/msfoundry/commit/extraction"
	"github.com/msfoundry/commit/store"
)

type NudgeTiers struct {
	Gentle string `json:"gentle"`
	Direct string `json:"direct"`
	Firm   string `json:"firm"`
}

func generateNudgeTiers(ctx context.Context, model string, c *store.Commitment) (*NudgeTiers, error) {
	prompt := fmt.Sprintf(`Write 3 WhatsApp follow-up messages for this situation, at different escalation levels:

- %s promised to: %s
- Context: %s
- Original quote: "%s"
- This was %s ago

Write exactly 3 versions:
1. GENTLE — casual, friendly check-in (1 sentence)
2. DIRECT — clear and specific ask (1-2 sentences)
3. FIRM — urgent, sets a deadline or consequence (1-2 sentences)

All should be natural WhatsApp messages. No greetings like "Hi" or "Hey there". No emojis.

Return JSON only: {"gentle":"...","direct":"...","firm":"..."}`, c.PersonName, c.Title, c.Context, c.SourceQuote, c.SourceTime)

	text, err := extraction.CallLocalLLM(ctx, model, prompt, 512)
	if err != nil {
		return nil, err
	}

	text = strings.TrimSpace(text)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		text = text[start : end+1]
	}

	var tiers NudgeTiers
	if err := json.Unmarshal([]byte(text), &tiers); err != nil {
		return &NudgeTiers{Gentle: text}, nil
	}
	return &tiers, nil
}

func generateNudgeMessage(ctx context.Context, model string, c *store.Commitment) (string, error) {
	prompt := fmt.Sprintf(`Write a short, natural WhatsApp follow-up message (1-2 sentences max) for this situation:

- %s promised to: %s
- Context: %s
- Original quote: "%s"
- This was %s ago

The message should be polite, casual, and natural — like something a real person would type on WhatsApp. Don't be formal or robotic. Don't use greetings like "Hi" or "Hey there". Just a friendly nudge about the thing.

Return ONLY the message text, nothing else.`, c.PersonName, c.Title, c.Context, c.SourceQuote, c.SourceTime)

	text, err := extraction.CallLocalLLM(ctx, model, prompt, 256)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func (s *Server) callNudgeWithFallback(ctx context.Context, apiKey string, c *store.Commitment) (string, error) {
	model := s.db.GetModel()
	text, err := generateNudgeMessage(ctx, model, c)
	if err != nil && strings.Contains(err.Error(), "model_not_found") && model != store.FallbackModel {
		log.Printf("model %s not available for nudge, falling back to %s", model, store.FallbackModel)
		s.db.SetModel(store.FallbackModel)
		return generateNudgeMessage(ctx, store.FallbackModel, c)
	}
	return text, err
}

func (s *Server) callNudgeTiersWithFallback(ctx context.Context, apiKey string, c *store.Commitment) (*NudgeTiers, error) {
	model := s.db.GetModel()
	tiers, err := generateNudgeTiers(ctx, model, c)
	if err != nil && strings.Contains(err.Error(), "model_not_found") && model != store.FallbackModel {
		log.Printf("model %s not available for nudge tiers, falling back to %s", model, store.FallbackModel)
		s.db.SetModel(store.FallbackModel)
		return generateNudgeTiers(ctx, store.FallbackModel, c)
	}
	return tiers, err
}
