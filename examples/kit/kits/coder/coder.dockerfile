# The image every agent on this kit runs, built by sbx env create. It starts from Docker's
# sandbox template (bash, the agent user, git, CA store); a real workload installs Hermes at
# the pinned commit here, and copies the seeds last so a seed edit rebuilds only those layers.
# troupe reads the pinned Hermes commit from this line; the build stage brings it in below.
ARG HERMES_GIT_SHA=0123456789abcdef0123456789abcdef01234567
FROM docker.io/docker/sandbox-templates:shell-docker@sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919
ARG HERMES_GIT_SHA
USER root
COPY --chown=1000:1000 files/home/ /home/agent/
RUN find /home/agent/.hermes -type d -exec chmod 700 {} + && find /home/agent/.hermes -type f -exec chmod 600 {} +
USER agent
