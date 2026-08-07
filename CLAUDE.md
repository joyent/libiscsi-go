# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go cgo wrapper around the `libiscsi` C library, exposing an iSCSI initiator (client) as idiomatic Go `io.Reader`/`io.Writer`/`io.Seeker`/`io.Closer` types. Module path: `github.com/joyent/libiscsi-go`.

## Development environment

This is a cgo package: it needs the `libiscsi` C headers/library and a C compiler to build, via `#cgo pkg-config: libiscsi` in `iscsi.go` and `callbacks.go`.

The repo includes a Flox manifest (`.flox/env/manifest.toml`) that pins `go`, `libiscsi`, and `gcc` — run `flox activate` to get a working toolchain. Without Flox, install `libiscsi-dev` (or equivalent) yourself and confirm cgo can find it with `pkg-config --exists libiscsi`.

## Common commands

```sh
go build ./...                                  # build everything
go vet ./...
go test ./...                                   # run the full test suite
go test . -run TestReadRandom -v                # single test
go test . -run 'TestReadCapacity16/3_TiB' -v    # single subtest
go test . -bench BenchmarkParallelSyncReaders -run ^$ -benchtime 5x   # benchmarks (concurrency_test.go)

# example CLI binaries (see cmd/)
go run ./cmd/example <initiator-iqn> <target-url>
go run ./cmd/drivefiller <initiator-iqn> <target-url> <percentage>
```

No real iSCSI target is required to run the test suite — tests spin up an in-process fake target (see Testing below).

## Architecture

### Core device wrapper (`iscsi.go`)

`device` wraps a `*C.struct_iscsi_context` and holds the connection's target/portal/LUN info. `New`/`Connect`/`Reconnect`/`Disconnect` manage the connection lifecycle; `Connect` retries with backoff (`retry-go`) and reinitializes the C context on failure, since libiscsi can leave the context in a bad state after a failed connection attempt.

SCSI commands (`Read16`, `Write16`, `ReadCapacity10/16`, `GetLBAStatus`) all follow the same two-mode pattern for issuing a libiscsi async task and waiting on it:

- **Sync**: build the task with the `_task` C function, pass the `syncCB` callback (a cgo trampoline into `iscsiSyncCB`), then block in `(*device).eventLoop`, which polls the iSCSI socket fd (`WhichEvents`/`GetFD`/`HandleEvents`) until the callback marks the shared `syncCallbackState` finished. This is the pattern most public methods use.
- **Async/channel**: pass `channelCB` instead, which delivers a `TaskResult` on a caller-provided `chan TaskResult` (see `Read16Async`, and its consumption pattern in `concurrency_test.go`). Useful for pipelining many in-flight reads on one connection.

Response parsing is done by hand, not via libiscsi's own `scsi_datain_unmarshall`: raw bytes from `task.datain` are read with `encoding/binary` (see `getReadCapacity10`, `getReadCapacity16`, `parseLBAStatusData`, and the `scsiTask` helper with its `getUint8/16/32` bounds-checked accessors). Keep new SCSI command parsing consistent with this style rather than introducing libiscsi's unmarshalling helpers.

**Concurrency**: a single `device` (and its `iscsi_context`) is *not* safe to use from multiple goroutines — it's a single-threaded C event loop. To parallelize, create multiple `device` connections (see the benchmarks in `concurrency_test.go` for the pattern: N devices, each with its own reader).

### Callback trampolines (`callbacks.go`)

cgo can't pass a Go function directly as a C function pointer. This file defines small C wrapper functions (`iscsiChannelCB_cgo`, `iscsiSyncCB_cgo`) that call the `//export`ed Go functions `iscsiChannelCB`/`iscsiSyncCB` (defined in `iscsi.go`), and exposes them as `C.iscsi_command_cb` values (`channelCB`, `syncCB`) for use when issuing tasks.

### Reader/Writer (`reader.go`, `writer.go`)

Both wrap a connected `*device` and translate byte-oriented `io.Reader`/`io.Writer` semantics into block-aligned `Read16`/`Write16` calls, using the LUN's block size and max LBA from `ReadCapacity16`. `WriteAt` requires block-aligned offsets and lengths; `ReadAt` handles arbitrary offsets/lengths, including partial trailing blocks and EOF clamping to the LUN's last LBA.

`Close()` on either type sets an `atomic.Bool` and waits on a `sync.WaitGroup` to drain any in-flight `Read`/`Write` calls before disconnecting the device — callers should expect `Close()` to block until pending I/O finishes (or run it in a goroutine).

`reader` additionally supports an opt-in `SkipSparseRegions()` fast path: it queries `GetLBAStatus` before a read, and if the entire requested block range is reported deallocated/anchored, it synthesizes zero bytes locally instead of transferring a real `Read16`. This is only enabled if the LUN reports `LBPRZ` (deallocated blocks guaranteed to read back as zero) in `READ CAPACITY(16)` — otherwise the call is a no-op, since it would be unsafe to fabricate data. Note the fake test target used in this repo never sets `LBPRZ`, so integration tests can only exercise the safe-fallback path, not the actual skip behavior.

### Testing

Tests use `github.com/gostor/gotgt` as an in-process fake iSCSI target instead of a real one. `iscsi_test.go` has the shared setup helpers: `createTargetTempfile`/`writeTargetTempfile` create a backing file, and `runTestTarget`/`createAndRunTestTarget` spin up a `gotgt` iSCSI service on a random free port (via `hashicorp/consul/sdk/freeport`) backed by that file, with `t.Cleanup` tearing it down. Most tests are black-box (`package iscsi_test`); `lba_status_test.go` is `package iscsi` for whitebox unit testing of the unexported byte-parsing helpers.

Because `gotgt`'s `SBCGetLbaStatus` implementation is a stub (it validates the request but never actually populates provisioning descriptors, and never sets `LBPRZ`), it can't be used to test real sparse-region behavior end-to-end — only the command round-trip and safe-fallback behavior.

### cmd/

Two demo CLI binaries built on this library, not part of the public API: `cmd/example` does a basic write-then-read against a LUN; `cmd/drivefiller` fills a given percentage of a LUN with random data in 1024-block chunks.
