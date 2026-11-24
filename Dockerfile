# Build a static binary, run it on distroless as non-root.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/ web/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tickets ./cmd/tickets \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build --chown=nonroot:nonroot /out/tickets /app/tickets
# distroless has no shell to mkdir at runtime: ship the data directory owned by nonroot.
COPY --from=build --chown=nonroot:nonroot /out/data /app/data
ENV HTTP_ADDR=:8080 \
    DB_PATH=/app/data/tickets.db
EXPOSE 8080
ENTRYPOINT ["/app/tickets"]
