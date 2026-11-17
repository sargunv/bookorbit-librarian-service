FROM docker.io/library/golang:1.27.3-alpine@sha256:bff16a94c0b4b6d9642581e2a3aa81beb3edb940b2f5a3aac1d22c6a68cb7747 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /librarian ./cmd/librarian

FROM docker.io/library/python:3.12-slim-bookworm@sha256:0f086103b026048cc50cb5da25ec435c023c5241a83fc44daea593377abc61c4
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /librarian /librarian
COPY tools/adobe/requirements.txt /opt/adobe/requirements.txt
RUN pip install --no-cache-dir -r /opt/adobe/requirements.txt && python -c "import urllib.request,hashlib,zipfile,io; b=urllib.request.urlopen('https://github.com/Leseratte10/acsm-calibre-plugin/releases/download/config/oscrypto_1.3.0_fork_2023-12-19.zip').read(); assert hashlib.sha256(b).hexdigest()=='0fcef6fe441e9b10760fbc944ae088ade00c18939cf55ecfb55a269993e5c317'; zipfile.ZipFile(io.BytesIO(b)).extractall('/tmp/oscrypto')" && pip install --no-cache-dir /tmp/oscrypto/oscrypto && rm -rf /tmp/oscrypto
COPY tools/adobe /opt/adobe
ENV PYTHONDONTWRITEBYTECODE=1
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/librarian"]
