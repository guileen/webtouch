#!/bin/sh
set -eu
hook="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/workbuddy-fetch-hook.cjs"
if [ -n "${NODE_OPTIONS:-}" ]; then
  export NODE_OPTIONS="${NODE_OPTIONS} --require=${hook}"
else
  export NODE_OPTIONS="--require=${hook}"
fi
exec /Applications/WorkBuddy.app/Contents/MacOS/Electron "$@"
