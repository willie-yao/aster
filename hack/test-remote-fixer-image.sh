#!/usr/bin/env bash
set -euo pipefail

image=${1:?usage: test-remote-fixer-image.sh IMAGE}

docker run --rm \
  --read-only \
  --user 65532:65532 \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=64m \
  --entrypoint /bin/sh \
  "$image" -c '
    set -eu
    test -x /usr/local/bin/server
    test -x /usr/local/bin/worker
    test -x /usr/local/bin/fetcher
    git --version
    for binary in opencode srt node npm; do
      if command -v "$binary" >/dev/null 2>&1; then
        echo "unexpected local execution tool: $binary" >&2
        exit 1
      fi
    done

    repo=/tmp/source
    patch=/tmp/change.patch
    mkdir -p "$repo"
    cd "$repo"
    git init -q
    git config user.name Fixture
    git config user.email fixture@example.test
    printf "before\n" > fixture.txt
    git add fixture.txt
    git commit -qm base
    printf "after\n" > fixture.txt
    git diff -- fixture.txt > "$patch"
    git checkout -q -- fixture.txt
    git apply --check "$patch"
    git apply --whitespace=nowarn "$patch"
    git add -A
    git diff --cached --check
    test "$(git diff --cached --name-only)" = fixture.txt
  '
