# syntax=docker/dockerfile:1

# ---- build the Go server with the game page embedded ----
FROM golang:1.26-alpine AS build
WORKDIR /src
# The game uses the standard library only, so there is no go.sum and nothing to download.
COPY go.mod ./
COPY main.go ./
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pagerpanic .
# The GPL text and the licence of every Go module linked in, staged by images/apps.sh.
COPY .dbcanvas-build/ /lic/
RUN sh /lic/go-third-party-licenses.sh "pagerpanic" . > /lic/THIRD_PARTY_LICENSES.txt

# ---- minimal runtime (single static binary, no shell needed) ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /pagerpanic /pagerpanic
COPY --from=build /lic/LICENSE /lic/THIRD_PARTY_LICENSES.txt /usr/share/doc/pagerpanic/
ENV PORT=8097
EXPOSE 8097
ENTRYPOINT ["/pagerpanic"]
