# The release image, built by goreleaser from binaries it already compiled:
# no compiler and no RUN step, so the image is the static binary on distroless
# and nothing else. `docker build .` at the repository root builds the same
# image from source (see /Dockerfile).
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/sindri /usr/local/bin/sindri
# A named volume mounted here inherits this ownership, so the non-root user
# can write blobs without a chown step the image has no shell to run.
COPY --chown=65532:65532 deploy/docker/data/ /var/lib/sindri/

USER 65532:65532
EXPOSE 8080
VOLUME ["/var/lib/sindri"]
ENTRYPOINT ["/usr/local/bin/sindri"]
CMD ["serve", "--config", "/etc/sindri/sindri.yaml"]
