FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/memebrary ./cmd/memebrary

FROM gcr.io/distroless/static-debian12:nonroot
ENV PORT=8080 DATA_DIR=/data DB_PATH=/data/memebrary.db MEDIA_DIR=/data/media
WORKDIR /app
COPY --from=build /out/memebrary /app/memebrary
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/memebrary"]
