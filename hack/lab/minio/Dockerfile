# MinIO (an S3 server with Object Lock) built from its official source: the project no longer publishes container images.
# Lab use only (hack/lab-audit-worm.sh). MINIO_REF pins the version.
FROM golang:1.27 AS build
ARG MINIO_REF=v0.0.0-20260212201848-7aac2a2c5b7c
RUN CGO_ENABLED=0 go install -trimpath -ldflags="-s -w" github.com/minio/minio@${MINIO_REF}
FROM gcr.io/distroless/static:nonroot
COPY --from=build /go/bin/minio /minio
USER 65532:65532
ENTRYPOINT ["/minio"]
