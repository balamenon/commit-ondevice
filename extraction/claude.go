package extraction

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/msfoundry/commit/store"
)

const defaultLocalLLMBaseURL = "http://127.0.0.1:8080/v1"
const defaultLocalEmbeddingBaseURL = "http://127.0.0.1:8081/v1"

type localLLMRequest struct {
	Model          string            `json:"model,omitempty"`
	MaxTokens      int               `json:"max_tokens"`
	Temperature    float64           `json:"temperature"`
	Messages       []localLLMMessage `json:"messages"`
	Stream         bool              `json:"stream"`
	DraftModel     string            `json:"draft_model,omitempty"`
	NumDraftTokens int               `json:"num_draft_tokens,omitempty"`
}

type localLLMMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type localLLMContentPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	ImageURL *localLLMImageURL `json:"image_url,omitempty"`
}

type localLLMImageURL struct {
	URL string `json:"url"`
}

type localLLMResponse struct {
	Choices []struct {
		Message json.RawMessage `json:"message"`
		Text    string          `json:"text"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type embeddingRequest struct {
	Model string   `json:"model,omitempty"`
	Input []string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func LocalLLMBaseURL() string {
	if baseURL := strings.TrimRight(os.Getenv("COMMIT_LLM_BASE_URL"), "/"); baseURL != "" {
		return baseURL
	}
	return defaultLocalLLMBaseURL
}

func CallLocalLLM(ctx context.Context, model, prompt string, maxTokens int) (string, error) {
	return callLocalChat(ctx, model, prompt, nil, "", maxTokens)
}

func CallLocalMultimodalDescription(ctx context.Context, model, prompt, path, mimeType string, maxTokens int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read media: %w", err)
	}
	dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return callLocalChat(ctx, model, prompt, dataURL, mimeType, maxTokens)
}

func callLocalChat(ctx context.Context, model, prompt string, imageDataURL any, mimeType string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	content := any(prompt)
	if imageURL, ok := imageDataURL.(string); ok && strings.HasPrefix(mimeType, "image/") {
		content = []localLLMContentPart{
			{Type: "text", Text: prompt},
			{Type: "image_url", ImageURL: &localLLMImageURL{URL: imageURL}},
		}
	}
	reqBody := localLLMRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: 0,
		Messages: []localLLMMessage{
			{Role: "user", Content: content},
		},
		Stream: false,
	}
	draftModel := os.Getenv("COMMIT_LLM_DRAFT_MODEL")
	if draftModel == "" {
		draftModel = store.DefaultDraftForModel(model)
	}
	if draftModel != "none" {
		reqBody.DraftModel = draftModel
		reqBody.NumDraftTokens = 3
	}
	if draftTokens := os.Getenv("COMMIT_LLM_NUM_DRAFT_TOKENS"); draftTokens != "" {
		var n int
		if _, err := fmt.Sscanf(draftTokens, "%d", &n); err == nil && n > 0 {
			reqBody.NumDraftTokens = n
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", LocalLLMBaseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv("COMMIT_LLM_API_KEY"); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("local llm call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("local llm error %d: %s", resp.StatusCode, string(respBody))
	}

	var result localLLMResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if result.Error != nil {
		return "", fmt.Errorf("local llm error: %s", result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty response from local llm")
	}

	if result.Choices[0].Text != "" {
		return result.Choices[0].Text, nil
	}
	return parseChatMessage(result.Choices[0].Message)
}

func CallLocalEmbeddings(ctx context.Context, model string, input []string) ([][]float64, error) {
	if len(input) == 0 {
		return nil, nil
	}
	reqBody := embeddingRequest{
		Model: model,
		Input: input,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	baseURL := strings.TrimRight(os.Getenv("COMMIT_EMBEDDING_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = defaultLocalEmbeddingBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv("COMMIT_LLM_API_KEY"); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("local embedding call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read embedding response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("local embedding error %d: %s", resp.StatusCode, string(respBody))
	}

	var result embeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse embedding response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("local embedding error: %s", result.Error.Message)
	}
	vectors := make([][]float64, 0, len(result.Data))
	for _, item := range result.Data {
		vectors = append(vectors, item.Embedding)
	}
	if len(vectors) != len(input) {
		return nil, fmt.Errorf("embedding count mismatch: got %d for %d inputs", len(vectors), len(input))
	}
	return vectors, nil
}

func parseChatMessage(raw json.RawMessage) (string, error) {
	var openAIMessage struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &openAIMessage); err == nil && openAIMessage.Content != "" {
		return openAIMessage.Content, nil
	}

	var mlxMessage string
	if err := json.Unmarshal(raw, &mlxMessage); err == nil && mlxMessage != "" {
		return mlxMessage, nil
	}

	return "", fmt.Errorf("empty response from local llm")
}

func callLocalLLM(ctx context.Context, _ string, model, prompt string) (string, error) {
	return CallLocalLLM(ctx, model, prompt, 2048)
}

// ModelNotFoundError is retained for compatibility with older fallback paths.
type ModelNotFoundError struct {
	Model string
}

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("model not found: %s", e.Model)
}
