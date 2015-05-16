#!/bin/bash
echo Running with args "${@}"

set -e
set -o pipefail
set -vx

GOPKG=github.com/mesosphere/kubernetes-mesos

test ${#} -eq 0 && CMD=( make bootstrap install ) || CMD=( "${@}" )
test -n "$GOPATH" || GOPATH=/pkg

# gopath is first directory in GOPATH : separated list
gopath="${GOPATH%%:*}"
pkg="${gopath}/src/${GOPKG}"

if [ -d $SNAP ]; then
  test ! -L "${pkg}" || /bin/rm -vf "${pkg}"  # remove any existing link
  parent=$(dirname "${pkg}")
  mkdir -pv "$parent"
  ln -sv $SNAP "$parent/$(basename $GOPKG)"
  cd "${pkg}"
  if [ "x${GIT_BRANCH}" != "x" ]; then
    if test -d '.git'; then
      git checkout "${GIT_BRANCH}"
    else
      echo "ERROR: cannot checkout a branch from non-git-based snapshot" >&2
      exit 1
    fi
  fi
else
  mkdir -pv "${pkg}"
  cd "${pkg}"
  git clone ${GIT_REPO:-https://${GOPKG}.git} .
  test "x${GIT_BRANCH}" = "x" || git checkout "${GIT_BRANCH}"
fi

exec "${CMD[@]}"
