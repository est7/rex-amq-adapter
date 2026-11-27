// Package fsutil holds the filesystem primitives every state writer shares:
// a lock (exclusive or shared) and an atomic replace.
package fsutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Flock takes an exclusive lock on path, polling until ctx ends.
func Flock(ctx context.Context, path string) (func(), error) {
	return flock(ctx, path, syscall.LOCK_EX)
}

// FlockShared takes a shared lock on path, polling until ctx ends: it waits
// for an exclusive holder and admits other shared ones.
func FlockShared(ctx context.Context, path string) (func(), error) {
	return flock(ctx, path, syscall.LOCK_SH)
}

// FlockDir takes an exclusive lock on directory dir itself (creating dir if
// absent), polling until ctx ends, without creating any lock file on disk.
func FlockDir(ctx context.Context, dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	return flockFD(ctx, f, dir, syscall.LOCK_EX)
}

func flock(ctx context.Context, path string, how int) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return flockFD(ctx, f, path, how)
}

func flockFD(ctx context.Context, f *os.File, path string, how int) (func(), error) {
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// WriteAtomic replaces path with data so readers see the old or the new
// content, never a partial file.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
