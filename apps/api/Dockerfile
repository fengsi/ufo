# Build context is the repo ROOT (the API needs db/migrations at runtime).
# Build:  docker build -f apps/api/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY apps/api/go.mod apps/api/go.sum ./apps/api/
RUN cd apps/api && go mod download
COPY apps/api ./apps/api
RUN cd apps/api && CGO_ENABLED=0 go build -o /out/api ./cmd/api

FROM alpine:3.22
WORKDIR /app
COPY --from=build /out/api /app/api
COPY db/migrations /app/db/migrations
ENV UFO_MIGRATIONS_DIR=/app/db/migrations
EXPOSE 8080
ENTRYPOINT ["/app/api"]
