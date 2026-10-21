# The agent's seeds, owned by the agent (uid 1000), folders 0700 and files 0600.
FROM docker.io/library/busybox:1.37 AS build
COPY files/home/ /out/home/agent/
RUN chown -R 1000:1000 /out/home/agent && find /out/home/agent/.hermes -type d -exec chmod 700 {} + && find /out/home/agent/.hermes -type f -exec chmod 600 {} +
FROM scratch
COPY --from=build /out/ /
