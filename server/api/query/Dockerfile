FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app ./query/...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=builder /app /app
ENTRYPOINT ["/app"]
