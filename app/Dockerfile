# Build-stage
FROM golang:1.22.7-alpine3.20 AS build
WORKDIR /src

# Go deps
COPY go.mod ./
RUN go mod download

# App code
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/puipr

# Run-stage (distroless, minimal)
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/puipr /app/puipr
COPY --from=build /src/templates /app/templates
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/puipr"]
