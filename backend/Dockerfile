FROM node:22-alpine AS frontend

WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run typecheck && npm run test:run && npm run build

FROM golang:1.27.3-alpine AS builder

WORKDIR /app

ENV CGO_ENABLED=0
ENV GOTOOLCHAIN=local

COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ .

RUN go build -trimpath -ldflags="-s -w" -o /bin/api ./cmd/api
RUN go build -trimpath -ldflags="-s -w" -o /bin/manage ./cmd/manage

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /bin/api /bin/api
COPY --from=builder /bin/manage /bin/manage
COPY --from=builder /app/web/static /app/web/static
COPY --from=frontend /frontend/dist /app/frontend
COPY backend/entrypoint.sh /app/entrypoint.sh
ENV SPA_DIR=/app/frontend
ENV STATIC_DIR=/app/web/static
# Checkout Windows dapat memakai CRLF; shebang Linux harus berakhir dengan LF.
RUN sed -i 's/\r$//' /app/entrypoint.sh && chmod +x /app/entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
