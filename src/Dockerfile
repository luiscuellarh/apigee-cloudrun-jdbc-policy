# Stage 1: Build static Go binary with pure-Go SQL drivers (Postgres, MySQL, SQL Server, Oracle, SQLite)
FROM golang:1.22-alpine AS builder
WORKDIR /app
RUN apk add --no-cache ca-certificates git
COPY go.mod ./
COPY *.go ./
COPY datasources.json ./datasources.json
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /jdbc-policy-connector .

# Stage 2: Minimal distroless runtime image
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /jdbc-policy-connector /jdbc-policy-connector
COPY --from=builder /app/datasources.json /etc/jdbc/datasources.json
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/jdbc-policy-connector"]
