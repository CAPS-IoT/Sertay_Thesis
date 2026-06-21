FROM golang:1.26 AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=1 go build -o sif-edge-host .

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /build/sif-edge-host .
COPY dht_reader.wasm .

EXPOSE 8080
CMD ["./sif-edge-host"]
