# Dovre's sandbox toolchain: built on dovre-sandbox (Node 24, git, curl), cached until this file changes.
FROM dovre-sandbox
COPY --from=golang:1.25 /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/home/agent/go/bin:$PATH
