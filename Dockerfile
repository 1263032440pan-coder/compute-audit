FROM --platform=$BUILDPLATFORM golang:1.26.1-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/audit-server ./cmd/audit-server

FROM scratch
COPY --from=build /out/audit-server /audit-server
USER 65532:65532
ENTRYPOINT ["/audit-server"]
