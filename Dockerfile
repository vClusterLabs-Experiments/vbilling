FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /vbilling ./cmd/vbilling

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /vbilling /vbilling
USER nonroot:nonroot
ENTRYPOINT ["/vbilling"]
