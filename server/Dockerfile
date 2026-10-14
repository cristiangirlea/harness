# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
FROM golang@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS build
WORKDIR /src
COPY go.mod ./
COPY internal/ ./internal/
COPY server/go.mod server/go.sum ./server/
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hrn-server ./cmd/hrn-server && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hrn-worker ./cmd/hrn-worker && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hrn-fixture-model ./cmd/hrn-fixture-model
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/hrn-server /usr/local/bin/hrn-server
COPY --from=build /out/hrn-worker /usr/local/bin/hrn-worker
COPY --from=build /out/hrn-fixture-model /usr/local/bin/hrn-fixture-model
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/hrn-worker"]
