package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestCleanStoredDescriptionJSON(t *testing.T) {
	raw := `{ "description": "A complete description.", "hashtags": ["x"]`
	if got := cleanStoredDescription(raw); got != "A complete description." {
		t.Fatalf("unexpected sanitized description: %q", got)
	}
}

func TestCreateListAndCursor(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "memebrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	first := Meme{ID: "00000000000000000000000000000001", Filename: "/tmp/one.png", OriginalName: "one.png", MimeType: "image/png", Size: 10, CreatedAt: "2026-01-01T00:00:00.000000001Z"}
	second := Meme{ID: "00000000000000000000000000000002", Filename: "/tmp/two.jpg", OriginalName: "two.jpg", MimeType: "image/jpeg", Size: 20, CreatedAt: "2026-01-01T00:00:00.000000002Z"}
	if err := s.Create(ctx, first, []string{"cats", "funny"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, second, []string{"dogs"}); err != nil {
		t.Fatal(err)
	}

	page, err := s.List(ctx, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Memes) != 1 || page.Memes[0].ID != second.ID || page.NextCursor == "" || page.Total != 2 {
		t.Fatalf("unexpected first page: %+v", page)
	}
	next, err := s.List(ctx, 1, page.NextCursor, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Memes) != 1 || next.Memes[0].ID != first.ID || next.NextCursor != "" {
		t.Fatalf("unexpected second page: %+v", next)
	}
	filtered, err := s.List(ctx, 10, "", "cat")
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 1 || len(filtered.Memes) != 1 || filtered.Memes[0].Tags[0] != "cats" {
		t.Fatalf("unexpected partial filtered result: %+v", filtered)
	}

	third := Meme{ID: "00000000000000000000000000000003", Filename: "/tmp/three.png", OriginalName: "three.png", MimeType: "image/png", Size: 30, CreatedAt: "2026-01-01T00:00:00.000000003Z"}
	if err := s.Create(ctx, third, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(ctx, first.ID, third.ID); err != nil {
		t.Fatal(err)
	}
	ordered, err := s.List(ctx, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered.Memes) != 3 || ordered.Memes[0].ID != first.ID || ordered.Memes[1].ID != third.ID || ordered.Memes[2].ID != second.ID {
		t.Fatalf("unexpected rearranged order: %+v", ordered.Memes)
	}
	if err := s.UpdateGeneratedContent(ctx, third.ID, "A generated description.", "ready", true, []string{"reaction", "dogs"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTags(ctx, third.ID, []string{"favorite", "dogs"}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Get(ctx, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "A generated description." || !updated.DescriptionGenerated || len(updated.Tags) != 3 {
		t.Fatalf("unexpected generated content: %+v", updated)
	}
	// A later worker/retry must not overwrite tags already supplied or generated.
	if err := s.UpdateGeneratedContent(ctx, third.ID, "Another description.", "ready", true, []string{"different"}); err != nil {
		t.Fatal(err)
	}
	updated, err = s.Get(ctx, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Tags) != 4 || updated.Tags[0] != "different" || updated.Tags[1] != "dogs" || updated.Tags[2] != "favorite" || updated.Tags[3] != "reaction" {
		t.Fatalf("generated tags were not merged: %+v", updated.Tags)
	}
	if deleted, err := s.Delete(ctx, third.ID); err != nil || deleted.ID != third.ID {
		t.Fatalf("delete meme: meme=%+v err=%v", deleted, err)
	}
	if _, err := s.Get(ctx, third.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected deleted meme to be absent, got %v", err)
	}
}
