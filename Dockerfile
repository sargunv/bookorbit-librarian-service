FROM docker.io/library/golang:1.27.3-alpine@sha256:bff16a94c0b4b6d9642581e2a3aa81beb3edb940b2f5a3aac1d22c6a68cb7747 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /librarian ./cmd/librarian

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /librarian /librarian
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/librarian"]
