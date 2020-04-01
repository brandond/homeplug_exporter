#!/bin/bash
set -euo pipefail

TOPLEVEL=`git rev-parse --show-toplevel`
REPONAME=`basename ${TOPLEVEL}`
TAG=${1:-latest}

docker build --force-rm -t ${USER}/${REPONAME}:${TAG} ${TOPLEVEL}
