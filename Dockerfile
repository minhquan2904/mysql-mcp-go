# Multi-stage build for the read-only MySQL MCP server.
#
# Build stage compiles a static, trimmed server binary (CGO off); the runtime
# stage is distroless static with a nonroot user, so the final image stays
# under 30MB with no shell or package manager inside.

FROM golang:1.26 AS build
WORKDIR /src
# Dependency layer first: cached unless go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/server"]
