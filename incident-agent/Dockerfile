# Digests pin the build and base; update them deliberately with the Go toolchain.
FROM golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/incident-api ./cmd/incident-api \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/incident-worker ./cmd/incident-worker

# No shell, package manager or other programs: the agent only needs its binaries and CA roots.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/incident-api"]
