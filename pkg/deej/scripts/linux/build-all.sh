#!/bin/sh

echo 'Building deej (all)...'

DIR="$(cd "$(dirname "$0")" && pwd)"
sh "$DIR/build-dev.sh"
sh "$DIR/build-release.sh"
