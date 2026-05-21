# ---- build stage ----
FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /dist/zk ./cmd/zk

# ---- runtime stage ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /dist/zk /usr/local/bin/zk

VOLUME ["/etc/zk", "/var/lib/zk"]
EXPOSE 25774

ENTRYPOINT ["zk", "run"]
