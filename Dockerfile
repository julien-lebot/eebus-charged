# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git make

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o eebus-charged .

# Runtime stage
FROM alpine:latest

# Install ca-certificates for HTTPS
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/eebus-charged .

# Create directories
RUN mkdir -p /app/certs

# Expose EEBUS port
EXPOSE 4712/tcp
EXPOSE 4712/udp

# Run as non-root user
RUN addgroup -S eebus && adduser -S eebus -G eebus
RUN chown -R eebus:eebus /app
USER eebus

ENTRYPOINT ["./eebus-charged"]
CMD ["run"]

