# Build stage
FROM golang:1.21-alpine AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/    api/
COPY controllers/ controllers/
COPY main.go main.go

RUN CGO_ENABLED=0 GOOS=linux go build -a -o srsran-operator ./main.go

# Runtime stage – distroless for a minimal attack surface
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/srsran-operator .
USER 65532:65532
ENTRYPOINT ["/srsran-operator"]
