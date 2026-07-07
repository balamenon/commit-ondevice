package extraction

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/msfoundry/commit/store"
)

const semanticBatchSize = 32

func (e *Extractor) IndexPendingMessages(ctx context.Context, limit int) error {
	model := e.db.GetEmbeddingModel()
	msgs, err := e.db.GetUnindexedMessages(model, limit)
	if err != nil {
		return err
	}
	return e.indexMessages(ctx, msgs)
}

func (e *Extractor) IndexPendingMedia(ctx context.Context, limit int) error {
	assets, err := e.db.GetUndescribedMediaAssets(limit)
	if err != nil {
		return err
	}
	if len(assets) == 0 {
		return nil
	}
	model := e.db.GetModel()
	embeddingModel := e.db.GetEmbeddingModel()
	for _, asset := range assets {
		description := mediaFallbackText(asset)
		if strings.HasPrefix(asset.MimeType, "image/") {
			prompt := `Describe this WhatsApp media for private semantic search and commitment tracking.

Focus on concrete people, objects, visible text, documents, screenshots, dates, amounts, tasks, places, and anything that could imply a commitment.
Return 3-6 concise sentences. Do not speculate beyond what is visible.`
			text, err := CallLocalMultimodalDescription(ctx, model, prompt, asset.Path, asset.MimeType, 512)
			if err != nil {
				log.Printf("media description failed for %s: %v", asset.ID, err)
			} else if strings.TrimSpace(text) != "" {
				description = strings.TrimSpace(text)
			}
		}
		if err := e.db.SetMediaDescription(asset.ID, description); err != nil {
			log.Printf("media description save failed for %s: %v", asset.ID, err)
		}
		vectors, err := CallLocalEmbeddings(ctx, embeddingModel, []string{"search_document: " + description})
		if err != nil {
			return err
		}
		if err := e.db.SaveSemanticDocument(&store.SemanticDocument{
			SourceType: "media",
			SourceID:   asset.ID,
			ChatJID:    asset.ChatJID,
			Title:      asset.FileName,
			Content:    description,
			Timestamp:  asset.Timestamp,
		}, embeddingModel, vectors[0]); err != nil {
			log.Printf("media semantic index save failed for %s: %v", asset.ID, err)
		}
	}
	return nil
}

func (e *Extractor) indexMessages(ctx context.Context, msgs []*store.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	model := e.db.GetEmbeddingModel()
	for start := 0; start < len(msgs); start += semanticBatchSize {
		end := start + semanticBatchSize
		if end > len(msgs) {
			end = len(msgs)
		}
		batch := msgs[start:end]
		inputs := make([]string, 0, len(batch))
		docs := make([]*store.SemanticDocument, 0, len(batch))
		for _, m := range batch {
			text := semanticMessageText(m)
			if text == "" {
				continue
			}
			inputs = append(inputs, "search_document: "+text)
			docs = append(docs, &store.SemanticDocument{
				SourceType: "message",
				SourceID:   m.ID,
				ChatJID:    m.ChatJID,
				Title:      m.ChatName,
				Content:    text,
				Timestamp:  m.Timestamp,
			})
		}
		if len(inputs) == 0 {
			continue
		}
		vectors, err := CallLocalEmbeddings(ctx, model, inputs)
		if err != nil {
			return err
		}
		for i, doc := range docs {
			if err := e.db.SaveSemanticDocument(doc, model, vectors[i]); err != nil {
				log.Printf("semantic index save failed for %s: %v", doc.SourceID, err)
			}
		}
	}
	return nil
}

func (e *Extractor) searchSemanticMessages(ctx context.Context, query string, limit int) ([]*store.Message, error) {
	model := e.db.GetEmbeddingModel()
	vectors, err := CallLocalEmbeddings(ctx, model, []string{"search_query: " + query})
	if err != nil {
		return nil, err
	}
	matches, err := e.db.SearchSemanticIndex(vectors[0], model, limit)
	if err != nil {
		return nil, err
	}
	msgs := make([]*store.Message, 0, len(matches))
	for _, match := range matches {
		if match.Score < 0.2 {
			continue
		}
		if match.SourceType != "message" && match.SourceType != "media" {
			continue
		}
		msgs = append(msgs, &store.Message{
			ID:        fmt.Sprintf("semantic:%s:%s", match.SourceType, match.SourceID),
			ChatJID:   match.ChatJID,
			ChatName:  match.Title,
			Content:   match.Content,
			Timestamp: match.Timestamp,
		})
	}
	return msgs, nil
}

func semanticMessageText(m *store.Message) string {
	content := strings.TrimSpace(m.Content)
	if content == "" {
		return ""
	}
	sender := m.SenderName
	if m.IsFromMe {
		sender = "Me"
	}
	parts := []string{
		"chat: " + m.ChatName,
		"sender: " + sender,
		"time: " + m.Timestamp.Format("2006-01-02 15:04"),
		"message: " + content,
	}
	return strings.Join(parts, "\n")
}

func mediaFallbackText(asset *store.MediaAsset) string {
	parts := []string{
		"WhatsApp media",
		"type: " + asset.MediaType,
		"mime: " + asset.MimeType,
	}
	if asset.FileName != "" {
		parts = append(parts, "file: "+asset.FileName)
	}
	if asset.Caption != "" {
		parts = append(parts, "caption: "+asset.Caption)
	}
	if info, err := os.Stat(asset.Path); err == nil {
		parts = append(parts, fmt.Sprintf("size: %d bytes", info.Size()))
	}
	return strings.Join(parts, "\n")
}
