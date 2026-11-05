FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/internal/videoworker ./api/internal/videoworker
COPY api/cmd/video-worker ./api/cmd/video-worker
RUN CGO_ENABLED=0 go build -trimpath -o /video-worker ./api/cmd/video-worker

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 worker && useradd --uid 10001 --gid 10001 --no-create-home worker
COPY --from=build /video-worker /usr/local/bin/video-worker
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/video-worker"]
CMD ["--root", "/scratch/incoming"]
