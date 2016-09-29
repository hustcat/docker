# This file describes the standard way to build Docker, using docker
#
# Usage:
#
# # Assemble the full dev environment. This is slow the first time.
# docker build -t docker .
#
# # Mount your source in an interactive container for quick testing:
# docker run -v `pwd`:/go/src/github.com/docker/docker --privileged -i -t docker bash
#
# # Run the test suite:
# docker run --privileged docker hack/make.sh test
#
# # Publish a release:
# docker run --privileged \
#  -e AWS_S3_BUCKET=baz \
#  -e AWS_ACCESS_KEY=foo \
#  -e AWS_SECRET_KEY=bar \
#  -e GPG_PASSPHRASE=gloubiboulga \
#  docker hack/release.sh
#
# Note: AppArmor used to mess with privileged mode, but this is no longer
# the case. Therefore, you don't have to disable it anymore.
#

FROM docker-dev:master

ENV DOCKER_BUILDTAGS apparmor seccomp selinux exclude_graphdriver_aufs

RUN rm -rf /go/src/github.com/docker/docker/*
# Upload docker source
COPY . /go/src/github.com/docker/docker
