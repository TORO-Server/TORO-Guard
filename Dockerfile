# toro-guard: TORO サーバーの荒らしを見張り、証拠の画像を付けて通報し、ロールバックする
#
#   docker build -t toro-guard .
#
# BlueMap の画面を撮るために、Chromium (chromedp/headless-shell) の上に実行ファイルを載せる。
# 実行ファイルは、ビルドする側の CPU で Go の交差コンパイルをして作る (QEMU の中でコンパイルしない)。

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS compile
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN go vet ./... \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /toro-guard .

# 単体の試験 (ビルドする側の CPU で動かす)。通らないと、下のイメージまで来ない
FROM --platform=$BUILDPLATFORM compile AS unit
RUN go test ./...

FROM chromedp/headless-shell:157.0.8103.27@sha256:7bee201478b2f6266aae01243740ee85c16281e32f42d3aed9fdf04f4eace7a1
COPY --from=compile /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# unit を経由する
COPY --from=unit /toro-guard /toro-guard
# 非 root で動かす。Docker のソケットは、compose の group_add で、そのグループに入れる。
# Chromium の作業場所 (プロフィール) は /tmp (tmpfs)。コンテナの中で Chromium のサンドボックスは使えないので no-sandbox
USER 65532:65532
ENV HOME=/tmp \
    DATA_DIR=/data \
    CHROME_PATH=/headless-shell/headless-shell \
    CHROME_FLAGS=no-sandbox,disable-dev-shm-usage,enable-unsafe-swiftshader,ignore-gpu-blocklist,use-angle=swiftshader,disable-gpu
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/toro-guard", "healthcheck"]
ENTRYPOINT ["/toro-guard"]
CMD ["run"]
