# Use a minimal base image with Go support
FROM golang:1.23 AS builder

# Set working directory inside the container
WORKDIR /app

# Copy go modules and install dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the application source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o vault-operator-helper main.go

# Use a minimal runtime image
FROM alpine:latest

# Install required utilities
RUN apk --no-cache add procps

# Set working directory
WORKDIR /

# Copy the compiled binary from the builder stage
COPY --from=builder /app/vault-operator-helper .

# Run the application
CMD ["/vault-operator-helper"]
