# Multi-stage build: frontend (Node) -> Go server (static, CGO off) -> Alpine runtime with FFmpeg.
# Builds natively on linux/arm64 (Oracle Ampere A1) and linux/amd64.

FROM node:24-alpine AS web
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM golang:1.27-alpine AS server
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=web /src/frontend/dist/ ./internal/web/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/dvfs ./cmd/server

FROM alpine:3.22
# ffmpeg from Alpine includes libx264 and the HLS/fMP4 muxers used for transcoding.
RUN apk add --no-cache ffmpeg ca-certificates tzdata wget \
 && addgroup -S dvfs && adduser -S -G dvfs -h /data dvfs \
 && mkdir -p /data && chown dvfs:dvfs /data
COPY --from=server /out/dvfs /usr/local/bin/dvfs
USER dvfs
ENV DVFS_DATA=/data TZ=Asia/Seoul
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/api/health || exit 1
ENTRYPOINT ["/usr/local/bin/dvfs"]
