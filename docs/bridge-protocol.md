# ModLock bridge protocol 1

This describes the currently implemented service. The lock parser accepts schema
1 and schema 2, but this bridge advertises and installs schema 1 only. Schema 2
application, full file-tree scanning, and publication are not implemented.

Start the bundled loader with an argument array, without a shell:

```text
modlock.exe bridge --protocol 1 --root <Minecraft instance directory>
```

The loader verifies and starts its adjacent `modlock-app.exe` for bridge calls.
It does not choose a payload from the shared CLI updater cache. The payload
bypasses automatic self-update, so startup does not mix CLI/menu output into JSON.
Qt must request capabilities and verify supported protocol, schema, and operations
before enabling a feature. Component updates must be coordinated by the launcher.

## Framing

stdin and stdout are UTF-8 NDJSON. One process accepts one request plus cancellation
messages. Keep stdin open while an operation is running if cancellation is needed.
Closing stdin after a request does not cancel it. Maximum input line size is 1 MiB.
Fragments are buffered until a full line arrives. A final input line without a
newline is accepted at EOF. Diagnostics go to stderr.

```json
{"type":"request","id":"request-1","operation":"capabilities"}
{"protocol":1,"type":"result","id":"request-1","result":{"protocol":1,"version":"dev","loader_protocol":1,"schemas":[1],"operations":["capabilities","read","install","verify","check","apply","scan","save-author-settings"],"cancel":true}}
```

Progress and the terminal event have the same request ID:

```json
{"protocol":1,"type":"progress","id":"request-1","message":"Получение ревизии сборки..."}
{"protocol":1,"type":"result","id":"request-1","error":{"code":"network","message":"..."}}
```

Cancel by sending `{"type":"cancel","id":"request-1"}`. The process waits
for the operation to stop safely. Before file mutations, cancellation aborts
preparation. During application, the operation completes or restores the files;
its final event is authoritative. Qt must not normally terminate or kill it.

## Operations

- `capabilities`: returns implemented operations, schema versions, and payload version.
- `read`: `params` contains `repository`, optional `branch`, `lock_path`, and
  `mods_dir`. Returns `revision` and `lock`; does not install files.
- `install`: `params` contains a `pack` source and the full previewed `revision`.
  It installs the schema 1 mods transactionally into an empty temporary instance.
- `verify`: checks the installed lock's managed mods without network access.
  Returns `state` (`healthy`, `unverified`, or `incomplete`), `needs_recovery`,
  and `missing`, `damaged`, and `unverified` filename lists. It accepts only
  regular non-empty files. A SHA-256 in the lock is compared with file contents;
  without a hash, presence and type are checked but content is not confirmed.
- `check`: reads the local installed lock and returns `revision`, `lock`, and
  `diff` with `added`, `removed`, `updated`, and `unchanged` mods, plus
  `local_installation` with the verify state. Missing or damaged managed mods
  appear in `diff.added` even when the remote commit is unchanged. Check does not
  modify files.
- `apply`: `params` contains the full 40-character `revision` returned by `check`.
  Fetches history and checks out that exact commit. A moved branch does not change
  the applied content. Missing and damaged managed mods are reinstalled even when
  the commit matches. Staged files must be regular, non-empty files and match any
  lock SHA-256 before the transaction begins. An unavailable commit fails instead
  of installing the tip.
- `save-author-settings`: writes `include_dirs`, `exclude_paths`, and
  `ignored_mod_ids` to `.modlock/author.toml`, separately from the published lock.
- `scan`: returns the installed JAR inventory, SHA-256 and sizes. Existing lock
  entries contribute their stable ID, version, and source. Unrecognized JARs
  remain unidentified for the author to classify. Arbitrary file-tree inventory
  and initial installation will arrive with the managed-file transaction.

```json
{"type":"request","id":"read-1","operation":"read","params":{"repository":"https://github.com/example/pack.git","branch":"main","lock_path":"mod.lock"}}
```

Errors contain a stable `code` and a displayable `message`. Codes are `network`,
`git_authorization`, `unsupported_format`, `unsupported_protocol`, `file_conflict`,
`instance_busy`, `recovery_failed`, `cancelled`, `invalid_request`, and `internal`.
Qt uses the code, never parses message text. A transport failure writing stdout
is reported on stderr and exits nonzero. Protocol/operation errors are terminal
JSON events; consumers must inspect the event instead of only the exit code.

Player repository fetching uses the embedded Go Git implementation, not installed
Git. Author publication will use installed Git and its credential manager/SSH.
