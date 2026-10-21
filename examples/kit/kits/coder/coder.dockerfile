# The image every agent on this kit runs, built by sbx env create. It starts from Docker's
# sandbox template (bash, the agent user, git, CA store); a real workload installs Hermes at
# this version here, and copies the seeds last so a seed edit rebuilds only those layers.
# troupe reads the Hermes version from this line; the build stage brings it in below.
ARG HERMES_VERSION=0.22.2
FROM docker.io/docker/sandbox-templates:shell-docker
ARG HERMES_VERSION
USER root
COPY --chown=1000:1000 files/home/ /home/agent/
RUN find /home/agent/.hermes -type d -exec chmod 700 {} + && find /home/agent/.hermes -type f -exec chmod 600 {} +
USER agent
