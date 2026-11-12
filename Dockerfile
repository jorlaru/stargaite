FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
ARG VERSION=dev
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/jorlaru/stargaite/internal/cli.Version=${VERSION}" \
      -o /out/stargaite ./cmd/stargaite \
 && mkdir /out/data

# distroless static, non-root (uid 65532): CA bundle, no shell.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/stargaite /stargaite
COPY --from=build --chown=65532:65532 /out/data /data
ENV STARGAITE_DATA_DIR=/data
USER 65532:65532
ENTRYPOINT ["/stargaite"]
CMD ["serve"]
