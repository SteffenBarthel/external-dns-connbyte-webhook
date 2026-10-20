FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/external-dns-connbyte-webhook .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/external-dns-connbyte-webhook /external-dns-connbyte-webhook
USER 65532:65532
EXPOSE 8888 8080
ENTRYPOINT ["/external-dns-connbyte-webhook"]
