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
	"unicode"

	"github.com/initialed85/memebrary-backend/internal/store"
)

type Generator struct {
	baseURL           string
	model             string
	apiKey            string
	client            *http.Client
	store             *store.Store
	reprocessExisting bool
	jobs              chan store.Meme
}

type GeneratedContent struct {
	Description string   `json:"description"`
	Hashtags    []string `json:"hashtags"`
	TextTags    []string `json:"text_tags"`
}

func New(baseURL, model, apiKey string, dataStore *store.Store, reprocessExisting bool) *Generator {
	return &Generator{
		baseURL:           strings.TrimRight(baseURL, "/"),
		model:             model,
		apiKey:            apiKey,
		client:            &http.Client{Timeout: 90 * time.Second},
		store:             dataStore,
		reprocessExisting: reprocessExisting,
		jobs:              make(chan store.Meme, 64),
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
	if g.reprocessExisting {
		if unprocessed, err := g.store.UnprocessedMetadata(ctx); err == nil {
			for _, meme := range unprocessed {
				g.Enqueue(meme)
			}
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
		_ = g.store.UpdateGeneratedContent(context.Background(), meme.ID, meme.Description, "failed", meme.DescriptionGenerated, nil)
	}
}

func (g *Generator) generate(ctx context.Context, meme store.Meme) {
	imageBytes, err := osReadFile(meme.Filename)
	if err != nil {
		g.markFailed(meme)
		return
	}
	result, err := g.request(ctx, meme.MimeType, imageBytes, meme.Description, meme.Tags)
	if err != nil {
		g.markFailed(meme)
		return
	}

	description := strings.TrimSpace(meme.Description)
	generatedDescription := false
	if description == "" {
		description = cleanText(result.Description)
		generatedDescription = description != ""
	}
	generatedTags := cleanTextTags(result.TextTags)
	if len(meme.Tags) == 0 {
		generatedTags = append(cleanTags(result.Hashtags, 8), generatedTags...)
	}
	// A missing description is still a failed generation. Any useful tags are
	// kept, so a retry can focus on the remaining missing field.
	status := "ready"
	if strings.TrimSpace(meme.Description) == "" && description == "" {
		status = "failed"
	}
	if err := g.store.UpdateGeneratedContent(context.Background(), meme.ID, description, status, generatedDescription, generatedTags); err != nil {
		return
	}
}

func (g *Generator) markFailed(meme store.Meme) {
	_ = g.store.UpdateGeneratedContent(context.Background(), meme.ID, meme.Description, "failed", meme.DescriptionGenerated, nil)
}

// osReadFile is a variable so the generator remains straightforward to test.
var osReadFile = func(name string) ([]byte, error) { return os.ReadFile(name) }

func (g *Generator) request(ctx context.Context, mimeType string, image []byte, existingDescription string, existingTags []string) (GeneratedContent, error) {
	imageURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(image)
	instruction := `Return only one valid JSON object, with exactly these fields: {"description":"one concise sentence","hashtags":["short-tag"],"text_tags":["visible-word"]}.
Describe what is visibly happening in the image. Hashtags must be 3-6 short lowercase visual or meme-context tags, without # or explanations. text_tags must contain every clearly readable word or short phrase visible in the image, kept as close to the exact spelling as possible, lowercase, without punctuation. Do not invent text and do not omit readable words. Use an empty array only when no text is visible.
If an existing description is supplied, copy it exactly into description. If existing hashtags are supplied, copy them exactly into hashtags. Always inspect the image for text_tags.`
	payload := map[string]any{
		"model": g.model,
		"messages": []any{
			map[string]any{
				"role":    "system",
				"content": "You analyze images for a private meme library. Never mention that you are an AI and never wrap JSON in markdown.",
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": instruction + "\nExisting description: " + strings.TrimSpace(existingDescription) + "\nExisting hashtags: " + strings.Join(existingTags, ", ")},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
				},
			},
		},
		// Qwen-style models otherwise spend the whole short completion budget in
		// hidden reasoning and leave message.content empty.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"response_format":      map[string]string{"type": "json_object"},
		"temperature":          0.2,
		"max_tokens":           240,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return GeneratedContent{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return GeneratedContent{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	response, err := g.client.Do(req)
	if err != nil {
		return GeneratedContent{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return GeneratedContent{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return GeneratedContent{}, fmt.Errorf("description endpoint returned %s", response.Status)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		return GeneratedContent{}, err
	}
	if len(completion.Choices) == 0 {
		return GeneratedContent{}, fmt.Errorf("description endpoint returned no choices")
	}
	return parseContent(completion.Choices[0].Message.Content)
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

func parseContent(raw json.RawMessage) (GeneratedContent, error) {
	text := cleanContent(raw)
	var result GeneratedContent
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		// Some compatible servers ignore response_format and add a tiny preamble.
		start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
		if start >= 0 && end > start {
			if jsonErr := json.Unmarshal([]byte(text[start:end+1]), &result); jsonErr == nil {
				return normalizeResult(result), nil
			}
		}
		if text == "" {
			return GeneratedContent{}, err
		}
		// Keep compatibility with a plain-sentence model response: the one call
		// still produces a useful description even if it misses the JSON contract.
		return GeneratedContent{Description: text}, nil
	}
	return normalizeResult(result), nil
}

func normalizeResult(result GeneratedContent) GeneratedContent {
	result.Description = cleanText(result.Description)
	result.Hashtags = cleanTags(result.Hashtags, 8)
	result.TextTags = cleanTextTags(result.TextTags)
	return result
}

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
		} else {
			text = string(raw)
		}
	}
	text = thinkBlock.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```json"))
	} else if strings.HasPrefix(text, "```") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```"))
	}
	text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	return strings.TrimSpace(text)
}

func cleanText(text string) string {
	text = strings.TrimSpace(strings.Trim(text, "\"'"))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}

func cleanTextTags(tags []string) []string {
	meaningful := make([]string, 0, len(tags))
	for _, raw := range tags {
		words := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool { return unicode.IsSpace(r) || r == '-' || r == '_' })
		kept := make([]string, 0, len(words))
		for _, word := range words {
			word = strings.Trim(word, ".,!?;:'\"()[]{}")
			if word != "" && !store.IsFillerWord(word) {
				kept = append(kept, word)
			}
		}
		if len(kept) > 0 {
			meaningful = append(meaningful, strings.Join(kept, "-"))
		}
	}
	return cleanTags(meaningful, 32)
}

func cleanTags(tags []string, max int) []string {
	seen := map[string]bool{}
	result := make([]string, 0, max)
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "#")))
		tag = strings.ReplaceAll(tag, " ", "-")
		if tag == "" || seen[tag] || len(result) >= max || len([]rune(tag)) > 40 {
			continue
		}
		valid := true
		for _, r := range tag {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
				valid = false
				break
			}
		}
		if valid {
			seen[tag] = true
			result = append(result, tag)
		}
	}
	return result
}
