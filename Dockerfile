FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/crawl4ai-mcp ./cmd/crawl4ai-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/crawl4ai-mcp /crawl4ai-mcp
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/crawl4ai-mcp"]
