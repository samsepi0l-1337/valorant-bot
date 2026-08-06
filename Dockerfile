# Multi-stage build for Linux server / Raspberry Pi (multi-arch via buildx).
#   docker build -t valorant-bot .
#   docker compose up -d

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/valorant-bot ./cmd/bot

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
  && addgroup -S valorant && adduser -S -G valorant -h /var/lib/valorant-bot valorant \
  && mkdir -p /var/lib/valorant-bot/data \
  && chown -R valorant:valorant /var/lib/valorant-bot
WORKDIR /var/lib/valorant-bot
COPY --from=build /out/valorant-bot /usr/local/bin/valorant-bot
USER valorant
EXPOSE 8787
ENV DATABASE_PATH=/var/lib/valorant-bot/data/bot.db
ENV AUTH_PORT=8787
ENV STORE_RESET_CRON="0 0 * * *"
ENTRYPOINT ["/usr/local/bin/valorant-bot"]
