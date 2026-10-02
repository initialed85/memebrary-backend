package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Port              string
	DataDir           string
	DBPath            string
	MediaDir          string
	MaxUploadSize     int64
	AIBaseURL         string
	AIModel           string
	AIAPIKey          string
	ReprocessExisting bool
	CORSOrigin        string
}

func Load() Config {
	dataDir := env("DATA_DIR", "./data")
	return Config{
		Port:              env("PORT", "8080"),
		DataDir:           dataDir,
		DBPath:            env("DB_PATH", filepath.Join(dataDir, "memebrary.db")),
		MediaDir:          env("MEDIA_DIR", filepath.Join(dataDir, "media")),
		MaxUploadSize:     envInt64("MAX_UPLOAD_BYTES", 20*1024*1024),
		AIBaseURL:         strings.TrimRight(envFirst("AI_BASE_URL", "OPENAI_BASE_URL"), "/"),
		AIModel:           envFirstDefault("AI_MODEL", "OPENAI_MODEL", "unsloth/Qwen3.6-35B-A3B-MTP-GGUF:UD-Q6_K_XL"),
		AIAPIKey:          envFirst("AI_API_KEY", "OPENAI_API_KEY"),
		ReprocessExisting: envBool("AI_REPROCESS_EXISTING"),
		CORSOrigin:        env("CORS_ORIGIN", "http://localhost:5173"),
	}
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envFirst(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func envFirstDefault(first, second, fallback string) string {
	if value := envFirst(first, second); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil || value < 1 {
		return fallback
	}
	return value
}
