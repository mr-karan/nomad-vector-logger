#!/bin/bash

echo "build binary"
make build

echo "building local image"
goreleaser --rm-dist --snapshot
docker tag ghcr.io/mr-karan/nomad-vector-logger:latest mr-karan/nomad-vector-logger:local

echo "deploying on nomad"
nomad run examples/deployment.nomad
