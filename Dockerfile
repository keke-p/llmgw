# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X llmgw/internal/version.Version=${VERSION}" \
      -o /out/llmgw ./cmd/llmgw \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llmgw /usr/local/bin/llmgw
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV LLMGW_LISTEN=:4000 \
    LLMGW_DB_PATH=/data/llmgw.db
VOLUME ["/data"]
EXPOSE 4000
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/llmgw", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/llmgw"]
CMD ["serve"]
