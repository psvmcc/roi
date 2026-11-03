FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG ROI_VERSION=dev
ARG ROI_COMMIT=unknown
ARG ROI_BUILD_DATE
RUN ROI_VERSION="$ROI_VERSION" ROI_COMMIT="$ROI_COMMIT" ROI_BUILD_DATE="$ROI_BUILD_DATE" sh scripts/build.sh /out/roi \
    && mkdir -p /out/data /out/tmp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/roi /roi
COPY --from=build --chown=10001:10001 /out/data /data
COPY --from=build --chmod=1777 /out/tmp /tmp
USER 10001:10001
WORKDIR /data
EXPOSE 8080 8081 8082
ENTRYPOINT ["/roi"]
CMD ["--config", "/config/config.yaml"]
