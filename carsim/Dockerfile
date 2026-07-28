# syntax=docker/dockerfile:1

# ---- build the Go server with the static frontend embedded ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /carsim .

# ---- minimal runtime (single static binary, no shell needed) ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /carsim /carsim
ENV PORT=8091
EXPOSE 8091
ENTRYPOINT ["/carsim"]
