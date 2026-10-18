# The agent's seeds, owned by the agent (uid 1000), folders 0700 and files 0600.
FROM docker.io/library/busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e AS build
COPY files/home/ /out/home/agent/
RUN chown -R 1000:1000 /out/home/agent && find /out/home/agent/.hermes -type d -exec chmod 700 {} + && find /out/home/agent/.hermes -type f -exec chmod 600 {} +
FROM scratch
COPY --from=build /out/ /
