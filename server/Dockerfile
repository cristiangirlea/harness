# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
FROM golang:1.26.11@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
WORKDIR /src
COPY go.mod ./
COPY internal/ ./internal/
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hrn ./cmd/hrn
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hrn-server ./cmd/hrn-server && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hrn-worker ./cmd/hrn-worker && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hrn-fixture-model ./cmd/hrn-fixture-model
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/hrn-server /usr/local/bin/hrn-server
COPY --from=build /out/hrn-worker /usr/local/bin/hrn-worker
COPY --from=build /out/hrn /usr/local/bin/hrn
ENV HRN_WORKER_HRN=/usr/local/bin/hrn
COPY --from=build /out/hrn-fixture-model /usr/local/bin/hrn-fixture-model
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/hrn-worker"]
