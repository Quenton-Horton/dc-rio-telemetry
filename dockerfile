# --- Stage 1: Build the Go Binary ---
FROM golang:1.27-alpine AS builder

WORKDIR /app

# Download dependencies first (caching optimization)
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code (including index.html)
COPY . .

# Compile a statically linked binary
RUN CGO_ENABLED=0 GOOS=linux go build -o telemetry-engine .

# --- Stage 2: Create the Lightweight Runner ---
FROM alpine:latest

# CA certificates are strictly required to make HTTPS calls to external APIs
RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Pull the compiled binary and HTML template from the builder stage
COPY --from=builder /app/telemetry-engine .
COPY --from=builder /app/index.html .

# Execute the engine
CMD ["./telemetry-engine"]
