package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/initialed85/memebrary-backend/internal/describe"
	"github.com/initialed85/memebrary-backend/internal/store"
)

type API struct {
	store      *store.Store
	generator  *describe.Generator
	mediaDir   string
	maxUpload  int64
	corsOrigin string
	logger     *slog.Logger
}

func New(dataStore *store.Store, generator *describe.Generator, mediaDir string, maxUpload int64, corsOrigin string, logger *slog.Logger) *API {
	return &API{store: dataStore, generator: generator, mediaDir: mediaDir, maxUpload: maxUpload, corsOrigin: corsOrigin, logger: logger}
}

func (a *API) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.corsOrigin != "" {
			origin := a.corsOrigin
			if origin == "*" || origin == r.Header.Get("Origin") {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		start := time.Now()
		defer func() {
			a.logger.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
		}()

		switch {
		case r.URL.Path == "/healthz":
			a.health(w, r)
		case r.URL.Path == "/api/memes" || r.URL.Path == "/api/memes/":
			a.memes(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/memes/"):
			a.memeAction(w, r)
		case strings.HasPrefix(r.URL.Path, "/media/"):
			a.media(w, r)
		default:
			notFound(w)
		}
	})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) memes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		result, err := a.store.List(r.Context(), limit, r.URL.Query().Get("cursor"), r.URL.Query().Get("tag"))
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		for i := range result.Memes {
			result.Memes[i].Filename = ""
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodPost:
		a.upload(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *API) upload(w http.ResponseWriter, r *http.Request) {
	if a.maxUpload < 1 {
		a.maxUpload = 20 * 1024 * 1024
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.maxUpload+1024*1024)
	if err := r.ParseMultipartForm(a.maxUpload + 1024*1024); err != nil {
		badRequest(w, "upload is too large or malformed")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		badRequest(w, "an image file is required")
		return
	}
	defer file.Close()
	if header.Size > a.maxUpload {
		badRequest(w, fmt.Sprintf("image must be smaller than %d MB", a.maxUpload/(1024*1024)))
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, a.maxUpload+1))
	if err != nil || int64(len(data)) > a.maxUpload {
		badRequest(w, "image is too large")
		return
	}
	mimeType := http.DetectContentType(data)
	if !allowedMIME(mimeType) {
		badRequest(w, "only JPEG, PNG, GIF, and WebP images are supported")
		return
	}
	if mimeType != "image/webp" {
		bounds, _, decodeErr := image.DecodeConfig(bytes.NewReader(data))
		if decodeErr != nil {
			badRequest(w, "the image could not be decoded")
			return
		}
		if bounds.Width > 12000 || bounds.Height > 12000 {
			badRequest(w, "image dimensions are too large")
			return
		}
	}

	id := randomID()
	extension := mimeExtension(mimeType)
	filename := id + extension
	if err := os.MkdirAll(a.mediaDir, 0o755); err != nil {
		serverError(w, err)
		return
	}
	temporary := filepath.Join(a.mediaDir, "."+filename+".upload")
	finalPath := filepath.Join(a.mediaDir, filename)
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		serverError(w, err)
		return
	}
	defer os.Remove(temporary)
	if err := os.Rename(temporary, finalPath); err != nil {
		serverError(w, err)
		return
	}

	description := strings.TrimSpace(r.FormValue("description"))
	tags := parseTags(r.FormValue("tags"))
	status := "none"
	if a.generator.Enabled() {
		// The vision pass now always extracts visible words for searchable tags,
		// even when the uploader supplied their own tags/description.
		status = "pending"
	}
	meme := store.Meme{
		ID:                id,
		Filename:          finalPath,
		OriginalName:      strings.TrimSpace(header.Filename),
		MimeType:          mimeType,
		Size:              int64(len(data)),
		Description:       description,
		DescriptionStatus: status,
		Tags:              tags,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if meme.OriginalName == "" {
		meme.OriginalName = "meme" + extension
	}
	if err := a.store.Create(r.Context(), meme, tags); err != nil {
		_ = os.Remove(finalPath)
		serverError(w, err)
		return
	}
	if status == "pending" {
		a.generator.Enqueue(meme)
	}
	created, err := a.store.Get(r.Context(), meme.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *API) memeAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "api" || parts[1] != "memes" {
		notFound(w)
		return
	}
	if len(parts) == 3 {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "DELETE")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.deleteMeme(w, r, parts[2])
		return
	}
	if parts[3] == "order" {
		if r.Method != http.MethodPatch {
			w.Header().Set("Allow", "PATCH")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.moveMeme(w, r, parts[2])
		return
	}
	if parts[3] != "describe" || r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	meme, err := a.store.Get(r.Context(), parts[2])
	if errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if !a.generator.Enabled() {
		badRequest(w, "AI descriptions are not configured")
		return
	}
	if err := a.store.UpdateDescription(r.Context(), meme.ID, meme.Description, "pending", meme.DescriptionGenerated); err != nil {
		serverError(w, err)
		return
	}
	meme.DescriptionStatus = "pending"
	a.generator.Enqueue(meme)
	writeJSON(w, http.StatusAccepted, meme)
}

func (a *API) moveMeme(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		BeforeID string `json:"before_id"`
		AfterID  string `json:"after_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		badRequest(w, "invalid move request")
		return
	}
	if input.BeforeID != "" && input.AfterID != "" {
		badRequest(w, "provide either before_id or after_id, not both")
		return
	}
	if input.BeforeID == id || input.AfterID == id {
		badRequest(w, "a meme cannot be moved relative to itself")
		return
	}
	var moveErr error
	if input.AfterID != "" {
		moveErr = a.store.MoveAfter(r.Context(), id, input.AfterID)
	} else {
		moveErr = a.store.Move(r.Context(), id, input.BeforeID)
	}
	if err := moveErr; errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deleteMeme(w http.ResponseWriter, r *http.Request, id string) {
	meme, err := a.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if _, err := a.store.Delete(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w)
			return
		}
		serverError(w, err)
		return
	}
	if err := os.Remove(meme.Filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.logger.Warn("remove deleted meme file", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) media(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/media/")
	if strings.Contains(id, "/") || len(id) != 32 {
		notFound(w)
		return
	}
	filename, err := a.store.Filename(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	file, err := os.Open(filename)
	if errors.Is(err, os.ErrNotExist) {
		notFound(w)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		serverError(w, err)
		return
	}
	meme, _ := a.store.Get(r.Context(), id)
	w.Header().Set("Content-Type", meme.MimeType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, filename, stat.ModTime(), file)
}

func allowedMIME(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func mimeExtension(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	default:
		return ".webp"
	}
}

func parseTags(input string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, 8)
	for _, raw := range strings.FieldsFunc(input, func(r rune) bool { return unicode.IsSpace(r) || r == ',' }) {
		tag := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "#")))
		if tag == "" || seen[tag] || len(result) >= 16 {
			continue
		}
		valid := true
		for _, r := range tag {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
				valid = false
				break
			}
		}
		if valid && len([]rune(tag)) <= 40 {
			seen[tag] = true
			result = append(result, tag)
		}
	}
	return result
}

func randomID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")[:16]
	}
	return hex.EncodeToString(bytes[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func badRequest(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": message})
}
func serverError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
}
func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}
