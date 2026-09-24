# syntax=docker/dockerfile:1.7
FROM golang:1.27-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/service ./cmd/server

FROM alpine:3.20
RUN adduser -D -u 10001 bot
WORKDIR /app
COPY --from=builder /app/bin/service /app/service
COPY persona.md /app/persona.md
RUN mkdir -p /app/data && chown -R bot:bot /app
USER bot
ENTRYPOINT ["/app/service"]
