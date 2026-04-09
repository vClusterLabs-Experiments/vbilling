FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /vbilling ./cmd/vbilling

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /vbilling /vbilling
USER nonroot:nonroot
ENTRYPOINT ["/vbilling"]
