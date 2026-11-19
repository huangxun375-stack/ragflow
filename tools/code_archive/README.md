# Code archive native dependencies

`prepare.sh` installs the official tree-sitter-language-pack **v1.13.3** shared
FFI and eleven grammars. Release archive SHA256 values are pinned in the script
and checked before extraction. It never modifies the Go module cache and does
not extract uploaded ZIP contents.

```bash
# Requires curl, tar, zstd, and sha256sum (or shasum).
bash tools/code_archive/prepare.sh

# Where GitHub release access needs a mirror, bytes still require the same hash.
CODE_ARCHIVE_ASSET_PREFIX=https://gh-proxy.com/ bash tools/code_archive/prepare.sh
```

The default install is `deps/code_archive/{native,grammars}`; downloaded release
archives are cached in `.cache/code_archive/v1.13.3`. A first argument overrides
the install directory. `CODE_ARCHIVE_DOWNLOAD_CACHE` overrides the download
cache. `RAGFLOW_TARGET_OS` and `RAGFLOW_TARGET_ARCH` select pinned Linux or macOS
assets for amd64 or arm64.

`build.sh` adds the shared library search path and runtime rpath. It requires the
FFI and all eleven grammars before compiling. For a custom install, set
`RAGFLOW_CODE_ARCHIVE_DEPS`; at runtime, set
`RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE` to its grammar directory. The production
Dockerfile prepares these dependencies in the Go build stage and copies them
into the final image. The binary has an additional relative runtime path,
`$ORIGIN/../deps/code_archive/native`, so the build host path is not required.

```bash
bash build.sh --cpp
bash build.sh --test ./internal/parser/codearchive \
  ./internal/ingestion/archiveartifact ./internal/service/archivepublication
bash build.sh --go
bin/ragflow_server --api --help
bin/ragflow_server --ingestor --help
```

The existing server CLI prints help and exits with status 1. Check the mode's
usage text as well as the exit status when using these two startup checks, or
run them as an integration test against the freshly built binary:

```bash
RAGFLOW_TEST_SERVER_BINARY=$PWD/bin/ragflow_server \
  bash build.sh --test-integration-go ./cmd -run '^TestServerBinaryStarts$'
```

With a dedicated Elasticsearch test service, the integration test verifies
source fields, unchanged code text, ready/stale/pending/orphan visibility,
pagination totals, and text retrieval through the real engine implementation:

```bash
ES_TEST=1 ES_HOST=http://localhost:9200 bash build.sh --test-integration-go \
  ./internal/engine/elasticsearch -run Archive
```

The normal repository native libraries and runtime resources remain prerequisites
for these commands; prepare them using the repository's existing development
dependency workflow. Missing grammars produce `ZIP_UNSUPPORTED_DEPLOYMENT`; the
processor does not download grammars to satisfy an ingestion request.

The checksum-pinned macOS assets are available, but native verification of this
feature targets Linux arm64.
