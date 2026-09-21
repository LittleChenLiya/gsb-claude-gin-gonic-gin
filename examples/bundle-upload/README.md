# Bundle Upload Example

Multi-file "bundle" upload with all-or-nothing visibility: a bundle
becomes downloadable only after **every** file in the request has been
fully written and validated, and the staging directory is atomically
renamed into the published root on the same filesystem.

## Design

- The server generates a bundle ID and creates a private staging
  directory at `<root>/.staging/<bundleID>` per request.
- Client file names are kept as display names only; objects on disk use
  server-generated keys. A `manifest.json` records display name, byte
  size, and SHA256 per file. Duplicate display names are fine; keys
  never collide. Empty files are recorded too.
- Limits: max files, max bytes per file, max total bytes. Totals are
  counted from bytes actually read, never from `Content-Length`.
- On success the whole staging directory is renamed to
  `<root>/<bundleID>` (same filesystem). On request cancel, read
  failure, limit overflow, or publish failure, only this request's
  staging directory is removed; concurrent uploads are unaffected.
- Downloads resolve `(bundleID, key)` against the published manifest
  only; forged IDs/keys and path traversal cannot reach other files.
- Startup cleanup removes stale entries inside `.staging` only, never
  follows symlinks, and never touches published bundles.

Durability scope: normal filesystem operations and visibility recovery
after a process restart are guaranteed; no promise is made about disk
persistence across power loss (no fsync), and no database is used.

## Run

```sh
go run ./examples/bundle-upload -addr :8080 -root ./bundle-data
```

## Upload

```sh
curl -F "files=@report.pdf" -F "files=@report.pdf" -F "files=@notes.txt" \
  http://localhost:8080/bundles
# => 201 {"id":"<bundleID>","files":[{"key":"...","name":"report.pdf",...}, ...]}
```

## Download

```sh
curl -OJ "http://localhost:8080/bundles/<bundleID>/files/<key>"
curl "http://localhost:8080/bundles/<bundleID>"   # manifest
```

## Restart and cleanup

If the process is interrupted mid-upload, the incomplete bundle stays
inside `<root>/.staging` and is never visible to the download API. On
the next start, entries in `.staging` older than `-staging-max-age`
(default 1h) are removed. Published bundles under `<root>` are never
touched by cleanup. To reclaim space immediately, stop the server and
delete `<root>/.staging` by hand.

## Tests

```sh
go test ./examples/bundle-upload/...
go test -race ./examples/bundle-upload/...
```
