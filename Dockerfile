FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bailucode2api ./cmd/server

FROM alpine:3.20
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=builder /bailucode2api /app/bailucode2api
RUN mkdir -p /data && chown app:app /data
USER app
EXPOSE 18790
ENV DATA_DIR=/data
ENTRYPOINT ["/app/bailucode2api"]
