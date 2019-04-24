#!/bin/bash

set -euxo pipefail

REMOTE_IP="$1"
VS_CODE_PORT=10010

result() {
  rv="$?"
  ssh "$REMOTE_IP" "pkill codessh" || true
  pkill sshcode || true
  exit "$rv"
}
trap "result" EXIT

go install 

sshcode --port="$VS_CODE_PORT" --no-open "$REMOTE_IP" &

ATTEMPTS=0
MAX_ATTEMPTS=10

# Try to curl VS Code locally on a backoff retry.
until curl -o /dev/null --connect-timeout 5 -s "localhost:$VS_CODE_PORT" || [ "$ATTEMPTS" -eq "$MAX_ATTEMPTS" ]; do
  sleep $(( ATTEMPTS++ ))
done

if [ "$ATTEMPTS" -eq "$MAX_ATTEMPTS" ];
then
  echo "Timed out waiting for code server to start"
  exit 1
fi

# Curl VS Code remotely.
ssh "$REMOTE_IP" "curl -o /dev/null --connect-timeout 5 -s localhost:$VS_CODE_PORT/"
