# Bundle Upload Example

Multi-file bundle upload with an all-or-nothing publish protocol. A bundle
becomes visible to the download endpoint only after **every** file in the
request has been fully written and validated; there is no window where the
first file is downloadable while a later one can still fail.

## How it works

1. The server generates a bundle ID and creates a private staging directory
   under `<root>/.staging/<id>` for each upload request.
2. Files are streamed to disk under server-generated storage keys. The
   client file name is kept only as display metadata. The manifest records
   display name, byte size and SHA-256 of every file (empty files included).
3. Limits (file count, per-file bytes, total bytes) are enforced by counting
   bytes actually read - `Content-Length` is never trusted.
4. After all files validate, the whole bundle is published with a single
   directory rename on the same filesystem into `<root>/bundles/<id>`.
5. Request cancellation, read errors, limit breaches or publish failures
   remove only this request's staging directory; concurrent uploads are
   unaffected.
6. The download endpoint resolves only keys listed in a published manifest;
   arbitrary disk paths and forged keys are rejected.

At startup, staging directories older than `-staging-ttl` are reclaimed
(crash recovery). Cleanup never follows symlinks and never touches the
published `bundles/` tree.

## Run

```sh
go run ./examples/bundle-upload -addr :8080 -root ./bundle-data
```

## Upload

```sh
curl -F "file=@a.txt" -F "file=@b.txt" http://localhost:8080/bundles
# => {"id":"<id>","files":[{"key":"file-000-...","name":"a.txt",...}]}
```

## Download

```sh
curl -OJ http://localhost:8080/bundles/<id>/files/<key>
```

## Restart and cleanup

If the process is killed mid-upload, the unfinished staging directory stays
under `<root>/.staging/` and the bundle simply never appears. On the next
start, directories older than `-staging-ttl` (default 1h) are deleted.
Published bundles survive restarts because they live in a separate tree and
are only moved there by an atomic rename.

## Guarantees

This example uses no database. It promises correct visibility under normal
filesystem operations and after process interruption (kill/restart). It
does **not** promise durability after a power loss: recently written files
may not be flushed to physical disk.

## Test

```sh
go test ./examples/bundle-upload/
go test -race ./examples/bundle-upload/   # requires cgo (gcc on Windows)
```
