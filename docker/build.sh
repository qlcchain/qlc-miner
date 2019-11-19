#!/bin/bash

network='live'

print_usage() {
	echo 'build.sh [-h]'
}

while getopts 'hn:' OPT; do
	case "${OPT}" in
		h)
			print_usage
			exit 0
			;;
		*)
			print_usage >&2
			exit 1
			;;
	esac
done

build_flag='build'

REPO_ROOT=`git rev-parse --show-toplevel`
COMMIT_SHA=`git rev-parse --short HEAD`
pushd $REPO_ROOT
echo ${build_flag}
docker build --build-arg BUILD_ACT="${build_flag}" -f docker/Dockerfile -t qlcchain/qlc-miner:latest .
popd
