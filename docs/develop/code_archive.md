# ZIP code archive backend

ZIP code snapshots are imported into the **Go backend** of RAGFlow v1.0.0 for
code question answering, code search and code analysis. One ZIP becomes one
document; its members are used only for chunk provenance and the file tree. This
change does not add Git cloning, public-link downloading, configuration/README
parsing or sensitive-information redaction. A trusted source preprocessing hook
is available for a later, separate requirement; its default is identity.

ZIP ingestion supports **MySQL metadata + Elasticsearch document storage**.
Other configured backends retain their ordinary-document behavior, but ZIP
upload and scheduling reject with `ZIP_UNSUPPORTED_DEPLOYMENT`; unsupported
backends do not require archive grammars at startup.

## Import and inspect

Use an existing dataset with a configured embedding model. The platform backend
retrieves local, KS3, or public-link files and uploads the ZIP bytes. Use one ZIP
per upload request so a retry cannot duplicate a partly accepted batch.

```bash
curl -X POST "$RAGFLOW_URL/api/v1/datasets/$DATASET_ID/documents?type=local" \
  -H "Authorization: Bearer $RAGFLOW_API_KEY" \
  -F 'file=@snapshot.zip'

curl -X POST "$RAGFLOW_URL/api/v1/datasets/$DATASET_ID/documents/parse" \
  -H "Authorization: Bearer $RAGFLOW_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"document_ids":["DOCUMENT_ID"]}'

curl "$RAGFLOW_URL/api/v1/datasets/$DATASET_ID/documents?id=$DOCUMENT_ID" \
  -H "Authorization: Bearer $RAGFLOW_API_KEY"

curl "$RAGFLOW_URL/api/v1/datasets/$DATASET_ID/documents/$DOCUMENT_ID/archive_tree" \
  -H "Authorization: Bearer $RAGFLOW_API_KEY"

curl "$RAGFLOW_URL/api/v1/datasets/$DATASET_ID/documents/$DOCUMENT_ID/chunks" \
  -H "Authorization: Bearer $RAGFLOW_API_KEY"
```

The upload creates **one document** with the internal `code_archive` parser. It
cannot be selected as a dataset default or substituted with a custom pipeline.
Same-name uploads create new documents, with independent stored object keys.

Poll `ingestion_status` (`CREATED`, `SCHEDULED`, `RUNNING`, `STOPPING`, `STOPPED`,
`COMPLETED`, `FAILED`) and the archive metadata. `archive_status=ready` identifies
the published result; `archive_run_id` identifies its parse run.
`archive_status` reports `stopped` as soon as a stop is requested, while the task
can still be `STOPPING`; use `ingestion_status` to decide when a stopped run is
terminal. Before publication, the tree endpoint returns `status=not_ready` and an
empty tree. Publication and every content read are checked against the current
run and attempt, including retrieval, neighboring chunks, citations, and
embedding compatibility checks.

Source chunks expose `source_path`, `start_line`, `end_line`, `code_lang`, and
`archive_run_id`. Source line numbers are one-based and inclusive. Paths are
relative to the ZIP. The first content line identifies the source location.
Tree statistics are diagnostic; the importing platform can keep filtering silent.
`tree` contains accepted source files and their parent directories only.
`stats.filtered_entries` optionally explains excluded paths with `path`, `type`
(`file` or `directory`), `reason`, and `file_count`. Reasons are `meta`, `dir`,
`gitignore`, `hidden`, `suffix`, `large`, `empty`, and `binary`, corresponding to
archive metadata, ignored directories, ignore rules, hidden entries, unsupported
suffixes, oversized files, empty files, and binary/undecodable files.

A rule-excluded directory is one entry covering all its skipped files; its reason
explains the directory exclusion, while existing per-file counters retain their
original first-match category. Thus a metadata file inside `node_modules` counts
as `skipped_meta`, but contributes to that directory's `file_count`. Empty
explicit directory entries are not counted or listed. Never infer whole-directory
exclusion from a single skipped child. Management UIs can merge these relative
paths into the accepted tree, show excluded nodes as read-only diagnostics, and
label directories with both accepted and filtered descendants as partially
filtered. Excluded paths have no chunks and do not enter retrieval.

Details are capped at 512 entries and 128 KiB of serialized entries, and may be
trimmed further to fit the 8 MiB execution artifact. When
`stats.filtered_entries_truncated=true`, only some paths are shown; file counters
remain complete. Counts on retained directory entries still include all of their
skipped files. Absence from a truncated list does not mean a path was accepted.
Details are stored in MySQL `stats_json` and follow the same publication and
permission checks as the tree, never appearing on a `not_ready` response.

## Lifecycle

| Action | Request | Notes |
|---|---|---|
| Start, or retry after `FAILED`/`STOPPED` | `POST /datasets/{id}/documents/parse` with `document_ids` | Check `success_count` |
| Clear results and reparse | `POST /datasets/{id}/chunks` with `document_ids` | Stop a running task and wait for a terminal state first |
| Stop | `POST /datasets/{id}/documents/stop` with `document_ids` | The worker settles the run as `STOPPED` |
| Delete | `DELETE /datasets/{id}/documents` with `ids` | Check `data.deleted` |

Deleting a document whose task is `RUNNING` or `STOPPING` is refused: the request
returns success with `deleted: 0` and leaves the run untouched. Stop the task,
wait for `ingestion_status=STOPPED`, then delete. Deletion removes the index rows,
tree, run snapshot and file references; attempt tombstones are kept so that a
late write from an interrupted worker is still removed.

## Configuration

Only two document settings are editable. When the chunk size is omitted, the
server's `default_chunk_token_num` supplies its initial value:

```json
{
  "parser_config": {
    "CodeArchive:Archive": {
      "chunk_token_num": 512,
      "code_file_overview": false
    }
  }
}
```

`chunk_token_num` must be an integer from 1 to 2048. The effective bound must also
fit the dataset embedding model, including the source header.

The fixed graph is `File -> CodeArchive -> Tokenizer`. It has no LLM Extractor,
RAPTOR or knowledge-compiler stage. The optional overview uses AST symbols,
not an LLM. ZIP chunk content is read-only through manual chunk APIs; reparse
the archive to update content. Document enable/disable remains supported.

Deployment policy is loaded from the `code_archive` section of
`service_conf.yaml` by both API and ingestor modes. These are the defaults:

```yaml
code_archive:
  max_archive_bytes: 10485760
  max_members: 5000
  max_depth: 20
  max_uncompressed_bytes: 104857600
  max_file_bytes: 1048576
  max_gitignore_bytes: 65536
  max_chunks: 100000
  artifact_max_bytes: 8388608
  ignored_dirs: []
  respect_gitignore: true
  syntax_error_ratio: 0.2
  default_chunk_token_num: 512
  chunk_target_ratio: 0.5
  test_source_weight: 0.95
  languages: [c, cpp, go, java, javascript, lua, python, tsx, typescript, vue]
  extensions: [.c, .cc, .cpp, .cxx, .go, .h, .hpp, .hxx, .java, .js, .jsx, .lh, .li, .ls, .lua, .py, .ts, .tsx, .vue]
```

All byte, file-count, depth and chunk-count limits must be positive and can
only be reduced from these caps. `ignored_dirs` adds directory basenames to
the fixed exclusions: `.git`, `.svn`, `.hg`, `node_modules`, `__pycache__`,
`.idea`, `.vscode`, `dist`, `build`, `target`, `vendor`, `.venv` and `venv`.
For example, `[generated]` also excludes every directory named `generated`.
Languages and extensions must be nonempty subsets of the supported lists;
a source file must match both. They cannot introduce an unprepared grammar
or add document/configuration file extensions. `syntax_error_ratio` and
`chunk_target_ratio` accept values from 0 to 1; `default_chunk_token_num`
accepts integers from 1 to 2048.

`test_source_weight` (in (0, 1]; 1 disables down-weighting) affects retrieval
only and is not frozen into parse runs. The remaining settings are parsing
policy.

If deduplicated syntax-error lines divided by file lines strictly exceeds
`syntax_error_ratio`, the ingestor emits one WARN for that file's final
line-splitting fallback. It includes `doc_id`, `kb_id`, `task_id`,
`archive_run_id`, `archive_attempt_id`, `source_path`, `code_lang`,
`error_lines`, `total_lines`, `error_ratio` and `threshold`, never source text.
C/C++ header retries and Vue script blocks do not emit separate warnings;
only the final file-level decision is logged. This is a recoverable fallback,
not a failed parse; `stats.syntax_fallbacks` still counts affected files.

Each setting can be overridden with `RAGFLOW_CODE_ARCHIVE_` followed by its
uppercase name. Values use JSON syntax: `RAGFLOW_CODE_ARCHIVE_MAX_MEMBERS=1000`,
`RAGFLOW_CODE_ARCHIVE_RESPECT_GITIGNORE=false`, or
`RAGFLOW_CODE_ARCHIVE_LANGUAGES='["go","python"]'`. Explicit empty, null,
mistyped and out-of-range values fail configuration loading. API upload checks
also apply the smaller of the archive cap and the general upload cap.

Use the same deployment policy on the API and ingestor. The effective parsing
policy and normalized document settings are frozen when the parse run is
queued, so a running or retried run keeps its original policy. Startup verifies
all selected grammars, including Vue's HTML and embedded JavaScript/TypeScript/
TSX dependencies and the C parser used for `.h` fallback. It never downloads
missing grammars during request processing.

## Safety and filtering

The server limits are:

| Limit | Value |
|---|---|
| Uploaded ZIP | 10 MiB (10,485,760 bytes) |
| File members | 5,000 |
| Path depth | 20 |
| Actual decompressed bytes read | 100 MiB |
| Individual source file | 1 MiB; larger files are skipped |
| Individual `.gitignore` | 64 KiB; larger rule files are skipped |
| Produced chunks | 100,000 |
| Serialized tree artifact | 8 MiB |

Members are read in memory, never extracted to disk. All directory entries are
checked before ordinary filtering: traversal, absolute/drive paths, control
characters, symlinks, duplicate normalized names, encrypted and inconsistent
headers reject the whole archive. Only single-volume, unprefixed ZIP/ZIP64 with
Store or Deflate members is accepted; the validated central directory must be
the one the reader uses. CRC failures while reading candidates fail the run.
Payloads skipped by path or size are not read solely to verify CRC.

Filtering covers archive metadata, dependency/build/VCS directories, nested
`.gitignore` rules, hidden files, unsupported suffixes, large/empty files and
binary content. Source text supports UTF-8 and GBK. Logical member names are
decoded separately from their original ZIP handles. Valid UTF-8 flags and
CRC-validated Info-ZIP Unicode Path metadata take precedence. Unflagged valid
UTF-8 is preserved; other names default to CP437. GBK repair requires CP437
box-drawing/Greek/math mojibake, Han characters and a lossless byte round-trip,
so ordinary CP437 names such as `grüße.py` and `éá.py` remain intact. Unmarked
CP437/GBK byte sequences are inherently ambiguous; use UTF-8 or Unicode Path
metadata for reliable disambiguation. Decoded collisions still fail.

The default policy respects nested `.gitignore` rules. Setting
`respect_gitignore: false` skips rule-file reads and matching. Oversized rule
files are skipped using their directory metadata; bytes actually read from
other rule files count toward the same total decompression budget as source
files. Matching cannot reinclude a file excluded by the fixed directory or
language/extension policy.

The tree's `root_name` uses the common top-level directory when all remaining
members share one, after excluding Mac metadata. Otherwise it uses the ZIP
filename without its extension. Member paths remain relative to the ZIP. Tree
nodes list directories first, then names, recursively.

File management can store ZIPs without parsing. Attachment, file association,
web import and connector paths reject or skip ZIP ingestion. This prevents the
new type classification from silently importing backups or release packages.

## Source chunking

Supported source families are Python, Go, Java, JS/JSX, TS/TSX, Vue, C/C++, and
Lua (`.lua/.lh/.ls/.li`). Vue scripts are parsed with their own language and
source offset; a script with another `lang` stays plain source text. C/C++ header
selection is explicit. Chunks follow code structure
and are merged to a target length:

- Structural units are top-level declarations (functions, methods, classes,
  structs, interfaces, enums, type definitions) with their adjacent leading
  comments, decorators and export/template wrappers. Blank-line-separated and
  trailing comments are not attached to the next declaration. Code between
  declarations is attached to the following unit; trailing code to the last one.
- Adjacent units in one file merge while the complete header plus body stays
  within `target = max(1, floor(chunk_token_num * chunk_target_ratio))` and both
  budgets (cl100k `chunk_token_num`, embedding model window). A unit over the
  target but within `chunk_token_num` stays whole and alone. An oversized class
  is split into its header and members, and members merge by the same rule.
  `chunk_target_ratio: 0` keeps every declaration in its own chunk.
- Declarations inside anonymous callables (arrow functions, function
  expressions, lambdas, Go `func` literals, unnamed Lua functions) belong to
  the enclosing statement and are never units or symbols. JavaScript/TypeScript
  class fields whose value is a callable are class members (`Class.field`).
- An oversized unit with no structural members is split by lines. Within the
  second half of each fitting line window, the cut is placed before the line
  whose starting syntax node is shallowest in the AST, never after a
  comment-only line or before a continuation token. Without a candidate the
  longest fitting prefix is kept.
- Syntax errors fall back to line-based chunking when the union of
  error-covered source lines exceeds `syntax_error_ratio` of the file's lines.
  Repeated errors on one line count once; long valid lines do not dilute the
  ratio. Vue files are split by lines. A missing grammar is a deployment error.
- Every chunk header lists the qualified names of all declarations it contains
  (truncated in the header only); the full list is `important_kwd`/`important_tks`.

Chunks never cross file boundaries. Every accepted file is reconstructed byte
for byte by concatenating its chunk bodies, line ranges are exact, no chunk is
whitespace-only, and output is deterministic. Any failure fails the whole
archive; no partial result is published.

## Embedding and retrieval

The archive embedder never truncates or rewrites code. Every input is counted
locally with the model's exact tokenizer; an input over the model window fails
the archive with `ZIP_TOKEN_LIMIT_EXCEEDED` before any provider call. A batch
rejected by the provider as over-limit, while every input is within the local
exact count, is bisected and re-embedded; a single rejected input is retried
once and then fails the archive, keeping the provider error in logs. The
ordinary-document shrink-and-retry path is not used for archives.

At retrieval, test-source chunks are down-weighted by `test_source_weight`
unless the question asks about tests. Test sources are recognized per language
(`_test.go`, `test_*.py`, `*.spec.ts`, `src/test/` and similar).

## Run snapshots and index cleanup

Enqueue persists an immutable `archive_run` snapshot before notifying NATS. It
contains the original object key, hash and size; normalized parser settings;
deployment parsing policy; and the resolved embedding identity and parameters.
API credentials are excluded. Retries read this saved snapshot, so document
renames and later parser-default changes cannot change the run's output.
Credential rotation is allowed; model binding, endpoint, tokenizer or
embedding-parameter changes require a new run. Publication rechecks the model
identity. ZIP embedding-cache reads and writes also include the frozen model
fingerprint; a missing or mismatched trusted identity bypasses caching rather
than using a reference-only entry. Ordinary-document cache keys are unchanged.

Each physical attempt has a durable `archive_attempt` inventory row. A worker
holds a 30-second lease and renews it every 10 seconds. The ingestor
periodically revokes expired attempts before deleting their exact index
generation. Current published output and live leases are protected. Reparse
atomically retires the previous published generation when it replaces the
publication pointer; a rolled-back enqueue preserves the previous pointer.
Cleanup failures remain retryable. Tombstones are retained and revisited to
catch an external write that arrives after a crashed worker's first cleanup,
including after document or dataset deletion.

Cancellation compensation releases the current attempt back to an invisible
pending pointer without failing its run. A persisted `STOPPING` request settles
as `STOPPED`; shutdown or lease interruption without a stop request remains
redeliverable, and another worker resumes the run after the lease expires.
Sweeping a released attempt does not fail the waiting run, and its old worker
cannot renew, publish or remove a replacement generation. A parse failure keeps
its specific reason in `archive_failure_reason`.

```yaml
code_archive_cleanup:
  interval_seconds: 300
  grace_seconds: 600
  batch_size: 100
```

The interval and grace must be between 1 and 86,400 seconds; batch size must be
between 1 and 10,000. The ingestor owns the scheduler and stops it with the
process. Parsing policy is frozen per run; this operational cleanup schedule is
read when the ingestor starts.

## Future preprocessing hook

The pure processor accepts `codearchive.SourceTransform`. The execution path
can inject it using `archiveartifact.WithSourceTransform(ctx, transform)` before
pipeline execution. The hook receives a decoded `SourceFile` and returns its
text/path plus an optional line map. If line count changes, supply one
`LineRange{Start, End}` per output line, mapping to original one-based lines.
Paths remain safe and unique, and the language selection cannot be changed.

No redaction rules, `security_config` API, sensitive-state machine, external
cleaning service, or separate original-file bucket is implemented here. A hook
being present is not a claim that its transformation removes sensitive values.

## Build and verify

See `tools/code_archive/README.md` for pinned native FFI and grammar preparation,
loader paths, the isolated development container, and actual-engine tests.
Prepare the repository's existing native/model resources first, then:

```bash
bash tools/code_archive/prepare.sh
bash build.sh --test ./internal/parser/codearchive \
  ./internal/ingestion/archiveartifact ./internal/service/archivepublication
bash build.sh --test ./internal/ingestion/component ./internal/ingestion/pipeline \
  ./internal/ingestion/task ./internal/service/document ./internal/service/chunk
bash build.sh --go
```

Run the normal database migration (`ragflow_server --migrate`) before starting
API and ingestor; it creates the archive tables.

Source metadata and publication predicates target Elasticsearch. Other document
engines retain their normal parsers but do not accept ZIP ingestion. Ordinary
reads on those engines receive neither ZIP publication predicates nor archive
source projections.

Parser and tokenizer regression inputs are small, original source snippets and
fixed-seed generated text in their owning test packages. ZIPs are constructed in
memory; the tests do not read third-party projects from the documentation tree.
The shared deterministic ZIP and 10 MiB input builders live in the repository's
existing `internal/ingestion/testutil` package.

Run parser/tokenizer regressions and opt-in scale checks through the build wrapper:

```bash
bash build.sh --test ./internal/parser/codearchive ./internal/tokenizer
bash build.sh --test ./internal/parser/codearchive -run '^$' \
  -bench '^BenchmarkArchive' -benchtime=1x -benchmem
bash build.sh --test-manual ./internal/parser/codearchive \
  -run '^TestArchiveTenMiBScale$' -v -timeout 10m
```

HTTP contracts use the existing `test/api` configuration and REST helpers.
Cross-service publication, restart/delete fencing and the optional complete
10 MiB deployment checks retain separate E2E/manual tags. See
`tools/code_archive/DEPLOYMENT.md` for their prerequisites and commands.

## Verification

The results below are historical validation measurements, not permanent output
counts or timing requirements for generated test inputs. The external openGauss
Go driver corpus used upstream commit `1b2ca1e407de54157316fcb4a808d8a87e007dac`
and ZIP SHA256 `319c5cdded1cf68f9a97386d1b52f596d7af66620576254495a42a890fffaa2a`.
That project archive is not required or distributed with the regression suite.

Verified on Linux ARM64 with Kvrocks 2.14.0, NATS 2.11 and SiliconFlow
`BAAI/bge-m3` (1,024 dimensions) on two metadata/document stacks: MySQL 8.0.40
with Elasticsearch 8.11.3 (password authentication, MinIO-compatible storage;
RAGFlow's default `docker/.env` versions), and MariaDB 11.4 with Elasticsearch
9.5.6 (SeaweedFS S3). The MySQL stack ran the migration twice (fresh and
repeated), the Elasticsearch archive integration suite and both HTTP scenarios
below; the parser and 10 MiB probes ran on the MariaDB stack:

- ZIP-related Go packages, the real Elasticsearch archive integration suite, the
  openGauss corpus and the 10 MiB parser probe (11,328 chunks, 10.9 s, 585 MB
  allocated, 308 MB peak RSS).
- The HTTP deployment scenario: upload, invisibility before publication and
  after stop/failure, byte reconstruction, stored vectors, retrieval, stopped
  reparse, asynchronous `ZIP_NO_SUPPORTED_FILES` and deletion.
- Restarting the ingestor while the openGauss archive was being parsed: the
  run resumed and published exactly one attempt; all 677 chunks reconstructed
  every file and every Elasticsearch row belonged to that run.
- Deleting during a run: refused while running; after the stop settled, the
  document was deleted and no index row, tree or retrieval result reappeared
  during at least 95 seconds of observation.

Retrieval was evaluated on 70 frozen Chinese questions over seven pinned public
repositories (development: Go, Python, TypeScript, C; holdout: Java, Go, Lua),
with hybrid 0.7 vector / 0.3 text scoring and no reranker. A question counts as
answered only when one chunk body contains the correct path and every answer
marker. With the defaults: development MRR@10 0.521, Hit@5 26/40; holdout
MRR@10 0.543, Hit@5 20/30. With only three or four repositories per set, these
are measurements on this benchmark rather than a general guarantee.

Non-ES ZIP ingestion is unsupported. Other CPU architectures and production
concurrency/load are not covered.
