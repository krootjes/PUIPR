# Build-stage
FROM golang:1.24-alpine3.20 AS build
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/puipr

# Run-stage
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/puipr /app/puipr
COPY --from=build /src/templates /app/templates
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/puipr"]
