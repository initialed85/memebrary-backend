# memebrary backend

Go HTTP API for meme/brary. It stores metadata in SQLite, image bytes in the configured media directory, and optionally queues image descriptions through an OpenAI-compatible vision endpoint.

```sh
go run ./cmd/memebrary
```

Important environment variables:

- `PORT` (default `8080`)
- `DATA_DIR` (default `./data`)
- `AI_BASE_URL` or `OPENAI_BASE_URL` (for example `http://192.168.137.111:8088/v1`)
- `AI_MODEL` / `OPENAI_MODEL`
- `AI_API_KEY` / `OPENAI_API_KEY`
- `MAX_UPLOAD_BYTES` (default 20 MiB)

Build the container with `docker build -t initialed85/memebrary-backend .`.
