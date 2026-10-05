FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder
# Filled in by BuildKit from --platform (or the host); no defaults, so a plain
# `docker build` on arm64 produces an arm64 binary.
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /vbilling ./cmd/vbilling

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /vbilling /vbilling
USER 65532:65532
ENTRYPOINT ["/vbilling"]
