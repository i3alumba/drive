FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/drive-api ./api/cmd/server

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends aria2 ca-certificates libreoffice \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 app \
    && mkdir -p /tmp/drive-torrents \
    && chown -R app:app /tmp/drive-torrents
USER app
COPY --from=build /out/drive-api /usr/local/bin/drive-api
EXPOSE 8080
ENTRYPOINT ["drive-api"]
