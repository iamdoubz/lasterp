### web build
FROM node:22-slim AS web
WORKDIR /src
RUN corepack enable
COPY package.json pnpm-workspace.yaml ./
COPY web/package.json web/
RUN pnpm install --frozen-lockfile=false
COPY web/ web/
RUN pnpm --dir web run build

### go build
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY cmd/ cmd/
COPY kernel/ kernel/
RUN CGO_ENABLED=0 go build -o /out/lasterp ./cmd/lasterp

### runtime
# ponytail: web/dist is shipped alongside the binary but not yet served by it
# (static serving lands with WP-0.6/1.5); single-binary embed is a later upgrade.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/lasterp /usr/local/bin/lasterp
COPY --from=web /src/web/dist /srv/web
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/lasterp", "serve"]
