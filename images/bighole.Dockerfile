# syntax=docker/dockerfile:1
#
# Big Hole — a third-party MongoDB FTDC viewer that runs entirely in the browser
# (MIT): https://github.com/zelmario/Big-hole
#
# Built from source at a pinned commit rather than pulled, because upstream
# publishes no image. A COMMIT, not a tag: the project has no tags and its
# package.json still says 0.0.0, so the revision is the only honest version
# there is — which is also why bigHoleRef in app/bighole.go carries it and the
# node's panel shows it.
#
# The two stages are upstream's own (node build → nginx serving the static
# output, with their nginx.conf), with one change: the build stage runs on the
# BUILD host's architecture and emits platform-neutral JavaScript, so an arm64
# installation does not run `npm ci` under emulation for output that would be
# byte-identical either way. Only the nginx runtime is pulled per platform.

ARG BIGHOLE_REF=896984fe9e9a7f1f59cc4ce29237625f8aaa713f

FROM --platform=$BUILDPLATFORM node:20-alpine AS build
ARG BIGHOLE_REF
RUN apk add --no-cache git
WORKDIR /src
# A full clone rather than a shallow fetch of the commit: the repository is small,
# and fetching a bare SHA depends on the server allowing it.
RUN git clone -q https://github.com/zelmario/Big-hole.git . \
    && git checkout -q "${BIGHOLE_REF}"
RUN npm ci
RUN npm run build

FROM nginx:alpine
COPY --from=build /src/dist /usr/share/nginx/html
COPY --from=build /src/docker/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
