# Runtime image for Angelia. Built by GoReleaser (dockers_v2): the build context
# holds the already-compiled static binaries under <os>/<arch>/, so there is no
# compile stage and no RUN step (no emulation is needed for multi-arch builds).
#
# distroless/static has no shell, no package manager and runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/angelia /usr/local/bin/angelia

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/angelia"]
