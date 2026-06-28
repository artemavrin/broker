# Build stage
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /broker ./cmd/broker

# Runtime stage
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /broker /app/broker
COPY migrations /app/migrations
EXPOSE 8080
ENTRYPOINT ["/app/broker"]
