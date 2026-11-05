# Runtime image for Angelia. Built by GoReleaser (dockers_v2): the build context
# holds the already-compiled static binaries under <os>/<arch>/, so there is no
# compile stage and no RUN step (no emulation is needed for multi-arch builds).
#
# distroless/static has no shell, no package manager and runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/angelia /usr/local/bin/angelia

USER 65532:65532

# No shell and no curl in the image: the probe is the binary itself, GET /healthz on the
# default listen port 8080 (docs/configuration.md). If you change `listen`, override the
# healthcheck with `angelia healthcheck --url http://127.0.0.1:<port>/healthz`.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/angelia","healthcheck"]

ENTRYPOINT ["/usr/local/bin/angelia"]
