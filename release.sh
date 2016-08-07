#!/bin/bash

VERSION="0.0.3"

echo "Building autopkgd version $VERSION"

mkdir -p pkg

build() {
  echo -n "=> $1-$2: "
  GOOS=$1 GOARCH=$2 go build -o pkg/autopkgd -ldflags "-X main.Version=$VERSION"
  du -h pkg/autopkgd
}

build "darwin" "amd64"
