#!/bin/sh

# DO NOT RUN LOCALLY - For docker build only!
# WILL NUKE YOUR .git !

apk update || exit 1
apk add git || exit 1

./build_protos.sh || exit 1

go get -v || exit 1
go build -ldflags "-X lib.version=`git rev-parse HEAD` -X lib.description=`git log -1 --pretty='format:%ai %s'`" || exit 1
go install || exit 1

rm -rf /go/src/*
rm -rf /go/pkg/*

apk del git
rm -rf /var/cache/apk/*
