package describe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/initialed85/memebrary-backend/internal/store"
)

type Generator struct {
	baseURL string
	model   string
	apiKey  string
	client  *http.Client
	store   *store.Store
	jobs    chan store.Meme
}

func New(baseURL, model, apiKey string, dataStore *store.Store) *Generator {
	return &Generator{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 90 * time.Second},
		store:   dataStore,
		jobs:    make(chan store.Meme, 64),
	}
}

func (g *Generator) Enabled() bool { return g.baseURL != "" }

func (g *Generator) Start(ctx context.Context) {
	if !g.Enabled() {
		return
	}
	for i := 0; i < 2; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case meme := <-g.jobs:
					g.generate(ctx, meme)
				}
			}
		}()
	}
	if pending, err := g.store.Pending(ctx); err == nil {
		for _, meme := range pending {
			g.Enqueue(meme)
		}
	}
}

func (g *Generator) Enqueue(meme store.Meme) {
	if !g.Enabled() {
		return
	}
	select {
	case g.jobs <- meme:
	default:
		_ = g.store.UpdateDescription(context.Background(), meme.ID, "", "failed", false)
	}
}

func (g *Generator) generate(ctx context.Context, meme store.Meme) {
	imageBytes, err := osReadFile(meme.Filename)
	if err != nil {
		_ = g.store.UpdateDescription(context.Background(), meme.ID, "", "failed", false)
		return
	}
	description, err := g.request(ctx, meme.MimeType, imageBytes)
	if err != nil || description == "" {
		_ = g.store.UpdateDescription(context.Background(), meme.ID, "", "failed", false)
		return
	}
	_ = g.store.UpdateDescription(context.Background(), meme.ID, description, "ready", true)
}

// osReadFile is a variable so the generator remains straightforward to test.
var osReadFile = func(name string) ([]byte, error) { return os.ReadFile(name) }

func (g *Generator) request(ctx context.Context, mimeType string, image []byte) (string, error) {
	imageURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(image)
	payload := map[string]any{
		"model": g.model,
		"messages": []any{
			map[string]any{
				"role":    "system",
				"content": "You describe images for a private meme library. Reply with exactly one concise, literal sentence describing what is visible and any readable text. Do not mention that you are an AI. Do not add a preamble or quotation marks.",
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Describe this meme in one short sentence."},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
				},
			},
		},
		// Qwen-style models otherwise spend the whole short completion budget in
		// hidden reasoning and leave message.content empty.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"temperature":          0.2,
		"max_tokens":           100,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	response, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("description endpoint returned %s", response.Status)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		return "", err
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("description endpoint returned no choices")
	}
	return cleanContent(completion.Choices[0].Message.Content), nil
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

func cleanContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			for _, part := range parts {
				if part.Text != "" {
					text += part.Text
				}
			}
		}
	}
	text = thinkBlock.ReplaceAllString(text, "")
	text = strings.TrimSpace(strings.Trim(text, "\"'"))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}
