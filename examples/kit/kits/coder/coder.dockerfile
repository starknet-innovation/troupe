# The image every agent on this kit runs, built by sbx env create. It starts from Docker's
# sandbox template (bash, the agent user, git, CA store); a real workload installs Hermes at
# this version here, and copies the seeds last so a seed edit rebuilds only those layers.
# troupe reads the Hermes version from this line; the build stage brings it in below.
ARG HERMES_VERSION=0.22.2
FROM docker.io/sbx/shell-docker-image:latest
ARG HERMES_VERSION
USER root
# What troupe reads from the published kit's files: the templates of an agent's own SOUL and
# .env, the Slack app manifest and a copy of the seeds. troupe/ is not copied: its publisher
# attaches it as the image's label (troupe kit label).
COPY templates/ /usr/share/troupe/
COPY slack-app-manifest.json /usr/share/troupe/
COPY files/home/.hermes/ /usr/share/troupe/seeds/
COPY --chown=1000:1000 files/home/ /home/agent/
RUN find /home/agent/.hermes -type d -exec chmod 700 {} + && find /home/agent/.hermes -type f -exec chmod 600 {} +
USER agent
