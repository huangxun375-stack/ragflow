# ZIP deployment verification

## Test layout

ZIP tests follow the repository's Go test layout:

- Parser, tokenizer, publication and handler regressions stay beside the code
  they exercise, in `*_test.go` files. Inputs are original source snippets and
  fixed-seed generated text; ZIP members are constructed in memory.
- HTTP contracts live in `test/api/code_archive_api_test.go` and use the existing
  `TestConfig`, REST request, upload, dataset and cleanup helpers.
- Complete API/ingestor/search scenarios are in `test/api` with
  `integration && e2e`. Worker restart and full-size provider checks also require
  `manual` and an explicit environment opt-in.
- The existing `internal/ingestion/testutil` package provides `ArchiveZIP` and
  `ArchiveScale` for tests that share deterministic archive generation. These
  helpers are not production dependencies.

The `tools/code_archive/prepare.sh` script only prepares checksum-pinned native
FFI and grammars. It is required by production builds, not a test fixture.

## Dedicated environment

Use a disposable deployment. API and ingestor must share migrated MySQL,
Elasticsearch, Kvrocks, NATS JetStream and a real S3-compatible object store.
Both processes require the normal runtime assets, and the grammar cache must be
prepared before startup. No handler, queue, storage, tokenizer, embedding driver
or search engine is replaced by a test double in these scenarios.

Use the existing API suite's configuration:

| Variable | Meaning |
| --- | --- |
| `HOST_ADDRESS` | RAGFlow API origin, such as `http://127.0.0.1:9380` |
| `RAGFLOW_TEST_API_KEY` | Credential for the disposable test tenant; the suite's normal login is used when unset |
| `RAGFLOW_TEST_EMAIL`, `RAGFLOW_TEST_PASSWORD` | Existing API-suite login inputs, if login is needed |
| `ES_HOST`, `ES_USERNAME`, `ES_PASSWORD` | Real Elasticsearch endpoint and optional Basic authentication |
| `CODE_ARCHIVE_E2E_MODEL_REFERENCE` | Optional full reference of an already configured embedding model; otherwise use the dataset default |
| `CODE_ARCHIVE_E2E_EMBEDDING_DIMENSIONS` | Expected vector dimensions; default `1024` |
| `CODE_ARCHIVE_E2E_TIMEOUT` | Optional whole-scenario timeout |
| `CODE_ARCHIVE_E2E_REPORT` | Optional JSON assertion report path; its parent must exist |

Provide credentials through the execution environment or an owner-readable file,
never checked-in files, URLs or command output. The ZIP tests reuse the API
suite's authenticated identity and model setup. They create and clean only their
own datasets/documents; they do not register another user, replace tenant model
defaults, or delete a pre-existing provider. The API suite's `TestMain` retains
its normal model preparation behavior, so use a tenant dedicated to testing.

Run API-only upload, configuration and restriction contracts:

```bash
bash build.sh --test-integration-go ./test/api \
  -run '^TestRESTCodeArchive' -v -timeout 10m
```

Run upload -> NATS -> real embedding -> Elasticsearch -> retrieval:

```bash
RAGFLOW_GO_TAGS=integration bash build.sh --test-e2e ./test/api \
  -run '^TestArchiveHTTPDeployment$' -v -timeout 20m
```

This scenario verifies the fixed parser, unpublished/stopped/failed visibility,
filtered-path diagnostics, source text and line ranges, stored vectors,
retrieval, a new generation after reparse, and deletion from the index and API.
Elasticsearch 8.x and 9.x are supported by the checks; only 9.2+ receives the
`_source.exclude_vectors=false` option.

## Worker restart and delete during a run

The manual test generates a moderate-size original Go archive at runtime. It
requires `CODE_ARCHIVE_E2E_RESTART=1` and a host-side observer capable of restarting
the isolated ingestor and inspecting its MySQL publication lease. No third-party
project or external ZIP is required.

`CODE_ARCHIVE_E2E_SIGNAL_DIR` must name a fresh shared directory. The observer is
an environment-specific prerequisite, not bundled infrastructure. It exchanges
these JSON messages with the test:

| Request | Required observer action | Reply |
| --- | --- | --- |
| `observe-running-a`, `observe-running-b` | Confirm a live running lease for the requested document | `running-a`, `running-b` |
| `restart-ingestor` | Restart while the requested document still owns that lease | `restarted`, including `restart_started_unix_ms` |
| `check-publication` | Confirm exactly one published attempt/run for the requested document | `publication-checked` |

Every reply must echo `document_id` and contain `"ok": true`; publication requests
also contain `run_id`. The optional `CODE_ARCHIVE_E2E_RESTART_REPORT` records the
dataset/document identity before fault injection for host-side failure cleanup.

```bash
CODE_ARCHIVE_E2E_RESTART=1 \
RAGFLOW_GO_TAGS='integration e2e' bash build.sh --test-manual ./test/api \
  -run '^TestArchiveHTTPWorkerRestartAndDeleteWhileRunning$' -v -timeout 25m
```

The test must observe an actual running lease before injection; an archive that
finishes before restart is not accepted as a successful restart test. It checks
all paginated source text and vectors, a unique published generation, refusal of
running deletion, deletion after terminal `STOPPED`, and at least 95 seconds
without late index rows, file trees or retrieval results.

## 10 MiB parser boundary

The manual parser check generates exactly 10,485,760 bytes with 192 Go,
TypeScript and Lua files, each below the 1 MiB member limit. It verifies admission
at the upload boundary and rejection above a reduced limit, source reconstruction,
line ranges, nonempty UTF-8 chunks, read accounting and complete-text cl100k and
BGE-M3 XLM-R token budgets.

```bash
bash build.sh --test-manual ./internal/parser/codearchive \
  -run '^TestArchiveTenMiBScale$' -v -timeout 10m
```

This is a correctness check, not a host-performance acceptance threshold. It does
not read Linux `/proc`, collect RSS, or depend on a particular machine's speed.
Use ordinary Go benchmarks for timing and allocation measurements:

```bash
bash build.sh --test ./internal/parser/codearchive -run '^$' \
  -bench '^BenchmarkArchivePerf$' -benchtime=1x -benchmem
```

## 10 MiB complete deployment

The full-size deployment check uses the same generated input with actual
`BAAI/bge-m3` embeddings. It consumes real provider tokens and requires both manual
isolation and `CODE_ARCHIVE_E2E_SCALE=1`. The model must already be configured;
local grammar and exact tokenizer preflight runs before the upload/provider work.
`CODE_ARCHIVE_E2E_SCALE_MAX_CHUNKS` (default `10000`) limits that work; choose an
explicit budget appropriate to the generated input instead of disabling the gate.

```bash
CODE_ARCHIVE_E2E_SCALE=1 \
CODE_ARCHIVE_E2E_SCALE_MAX_CHUNKS=12000 \
CODE_ARCHIVE_E2E_TIMEOUT=30m \
RAGFLOW_GO_TAGS='integration e2e' bash build.sh --test-manual ./test/api \
  -run '^TestArchiveHTTPScaleDeployment$' -v -timeout 35m
```

The dataset's configured model must resolve to `BAAI/bge-m3` for this
model-specific preflight. `RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE` must point to the
prepared grammar directory. The optional
`CODE_ARCHIVE_E2E_SCALE_REPORT` distinguishes all checked source pages/chunk totals
from sampled stored vectors. Provider billing can also include query embeddings.
Manual tests are opt-in and are not part of CI or default API tests.
