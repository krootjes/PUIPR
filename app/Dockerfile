# Build
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/puipr

# Run
FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY --from=build /out/puipr /app/puipr
COPY --from=build /src/templates /app/templates
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/puipr"]
