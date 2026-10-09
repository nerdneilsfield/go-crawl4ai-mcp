FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/crawl4ai-mcp /crawl4ai-mcp
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/crawl4ai-mcp"]
