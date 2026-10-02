package store

import (
	"context"
	"path/filepath"
	"testing"
)

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
	filtered, err := s.List(ctx, 10, "", "cats")
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 1 || len(filtered.Memes) != 1 || filtered.Memes[0].Tags[0] != "cats" {
		t.Fatalf("unexpected filtered result: %+v", filtered)
	}
}
