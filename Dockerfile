# toro-guard: TORO サーバーの荒らしを見張り、証拠の画像を付けて通報し、ロールバックする
#
#   docker build -t toro-guard .
#
# イメージの中身は、実行ファイルと CA 証明書だけ。シェルも Chromium も入れない。
# 実行ファイルは、ビルドする側の CPU で Go の交差コンパイルをして作る (QEMU の中でコンパイルしない)。

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS compile
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# timetzdata: イメージに時刻帯のファイルが無いので、TZ を読めるように実行ファイルに入れる
RUN go vet ./... \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags timetzdata -ldflags="-s -w -X main.version=${VERSION}" -o /toro-guard .

# 単体の試験 (ビルドする側の CPU で動かす)。通らないと、下のイメージまで来ない
FROM --platform=$BUILDPLATFORM compile AS unit
RUN go test ./...

FROM scratch
COPY --from=compile /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# unit を経由する
COPY --from=unit /toro-guard /toro-guard
# 非 root で動かす。ソケットを使うのは toro-console (console-relay) だけで、compose の group_add でそのグループに入れる
USER 65532:65532
ENV DATA_DIR=/data
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/toro-guard", "healthcheck"]
ENTRYPOINT ["/toro-guard"]
CMD ["run"]
