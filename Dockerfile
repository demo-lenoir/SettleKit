FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/settlekit ./cmd/settlekit

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/settlekit /settlekit
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/settlekit"]
