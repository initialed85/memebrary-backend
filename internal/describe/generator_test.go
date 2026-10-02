package describe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/initialed85/memebrary-backend/internal/store"
)

func TestRequestProducesStructuredDescriptionAndHashtags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ResponseFormat map[string]string `json:"response_format"`
			Messages       []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if payload.ResponseFormat["type"] != "json_object" {
			t.Errorf("missing JSON response format: %#v", payload.ResponseFormat)
		}
		var multimodal []struct {
			Type string `json:"type"`
		}
		if len(payload.Messages) != 2 || json.Unmarshal(payload.Messages[1].Content, &multimodal) != nil || len(multimodal) != 2 {
			t.Errorf("unexpected multimodal messages: %#v", payload.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"description\":\"A reaction image.\",\"hashtags\":[\"reaction\",\"funny\",\"reaction\"],\"text_tags\":[\"HELLO\",\"world\"]}"}}]}`))
	}))
	defer server.Close()

	dataStore, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "memebrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	generator := New(server.URL, "test-model", "", dataStore, false, 1)
	result, err := generator.request(context.Background(), "image/png", []byte("image"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Description != "A reaction image." || len(result.Hashtags) != 2 || result.Hashtags[0] != "reaction" || len(result.TextTags) != 2 || result.TextTags[0] != "hello" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestParsePartialContentSalvagesTags(t *testing.T) {
	result := parsePartialContent(`{"description":"A picture.","hashtags":["meme","funny"],"text_tags":["HELLO","WORLD"`)
	if result.Description != "A picture." || len(result.Hashtags) != 2 || len(result.TextTags) != 2 {
		t.Fatalf("unexpected partial result: %+v", result)
	}
}

func TestTextTagsSkipFillerWords(t *testing.T) {
	result := cleanTextTags([]string{"the", "1", "#2", "we are fucked", "out of office", "OpenAI"})
	if len(result) != 3 || result[0] != "fucked" || result[1] != "office" || result[2] != "openai" {
		t.Fatalf("unexpected text tags: %#v", result)
	}
}

func TestParseContentAcceptsMarkdownWrappedJSON(t *testing.T) {
	result, err := parseContent(json.RawMessage("```json\n{\"description\":\"A cat.\",\"hashtags\":[\"#Cats\",\"cute cats\"]}\n```"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Description != "A cat." || len(result.Hashtags) != 2 || result.Hashtags[0] != "cats" || result.Hashtags[1] != "cute-cats" {
		t.Fatalf("unexpected normalized result: %+v", result)
	}
}
