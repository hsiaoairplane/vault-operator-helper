# Use a minimal base image with Go support
FROM golang:1.26 AS builder

# Set working directory inside the container
WORKDIR /app

# Copy go modules and install dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the application source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o vault-operator-helper .

# Use a minimal runtime image. Pin to a specific minor version instead of
# :latest for reproducible, supply-chain-friendly builds.
FROM alpine:3.21

# Install required utilities (pgrep/kill live in procps)
RUN apk --no-cache add procps

# Set working directory
WORKDIR /

# Copy the compiled binary from the builder stage
COPY --from=builder /app/vault-operator-helper .

# Health/readiness probe server (see -health-addr).
EXPOSE 8081

# NOTE: the process is intentionally left running as root. It signals the main
# container's process across a shared process namespace, which requires matching
# privileges; running as an unprivileged user would break the restart on most
# setups. See README.md for deployment details.

# Run the application
CMD ["/vault-operator-helper"]
