# payments-svc and payments-portal: multi-stage, distroless, non-root.
#
# Build context is the repository root (the service shares the module with
# the model's back office):
#   docker build -f services/payments-svc/Dockerfile -t payments-svc .
#   docker build -f services/payments-svc/Dockerfile --build-arg BUILD_TAGS=demo -t payments-svc:demo .
#
# A production build (BUILD_TAGS empty) refuses static tokens, SQLite,
# loopback webhooks and the demo encryption key; give it an OIDC issuer, a
# Postgres DSN, a secret key and an Ethereum RPC URL. The image carries the
# Foundry artifacts from contracts/out, so run `forge build` first.

FROM golang:1.26.8-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY services/ services/
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -tags "${BUILD_TAGS}" -ldflags="-s -w -buildid=" \
      -o /out/payments-svc ./services/payments-svc/cmd/payments-svc \
 && CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w -buildid=" \
      -o /out/ofac-download ./services/payments-svc/cmd/ofac-download \
 && if [ "${BUILD_TAGS}" = "demo" ]; then CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -tags demo \
      -ldflags="-s -w -buildid=" -o /out/payments-portal ./services/payments-portal/cmd/payments-portal; fi
RUN mkdir -p /out/data && chmod -R u=rwX,go=rX services/payments-svc/configs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/ /usr/local/bin/
COPY --from=builder /src/services/payments-svc/configs/ /etc/payments-svc/
COPY contracts/out/ /opt/contracts/out/
COPY --from=builder --chown=nonroot:nonroot /out/data/ /var/lib/payments-svc/
ENV CONTRACT_ARTIFACTS=/opt/contracts/out OFAC_DIR=/var/lib/payments-svc/ofac HTTP_ADDR=0.0.0.0:8090 GRPC_ADDR=0.0.0.0:9090
USER nonroot:nonroot
EXPOSE 8090 9090
ENTRYPOINT ["/usr/local/bin/payments-svc"]
CMD ["-conf", "/etc/payments-svc"]
