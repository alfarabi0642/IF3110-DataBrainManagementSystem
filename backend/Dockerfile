FROM golang:alpine AS builder

WORKDIR /app

ENV CGO_ENABLED=0
ENV GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -trimpath -ldflags="-s -w" -o /bin/api ./cmd/api
RUN go build -trimpath -ldflags="-s -w" -o /bin/manage ./cmd/manage

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /bin/api /bin/api
COPY --from=builder /bin/manage /bin/manage
COPY --from=builder /app/web /app/web
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
