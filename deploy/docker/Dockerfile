# The release image, built by goreleaser from binaries it already compiled:
# no compiler and no RUN step, so the image is the static binary on distroless
# and nothing else. `docker build .` at the repository root builds the same
# image from source (see /Dockerfile).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

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
