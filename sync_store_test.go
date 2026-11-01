package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func testStore(t *testing.T) *redisSyncStore {
	t.Helper()
	server := miniredis.RunT(t)
	store, err := openSyncStore(context.Background(), Config{RedisURL: "redis://" + server.Addr(), RedisPrefix: "test:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.client.Close() })
	return store
}
func TestRedisSyncLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, ok, err := s.reserve(ctx, "/shows/A", "queued")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, p := range []string{"/shows", "/shows/A", "/shows/A/season", "/"} {
		if _, ok, err := s.reserve(ctx, p, "removing"); err != nil || ok {
			t.Fatalf("conflict %s: %v %v", p, ok, err)
		}
	}
	if _, ok, err := s.reserve(ctx, "/shows/AB", "removing"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.stop(ctx, "/shows/A"); err != nil {
		t.Fatal(err)
	}
	r, err := s.update(ctx, id, "claim")
	if err != nil || !r.Stop {
		t.Fatal(r, err)
	}
	if _, err := s.update(ctx, id, "claim"); err == nil {
		t.Fatal("duplicate claim succeeded")
	}
	data, _ := json.Marshal(SyncResult{Progress: 42, Speed: 1000})
	r, err = s.update(ctx, id, "progress", string(data))
	if err != nil || !r.Stop || r.Progress != 42 {
		t.Fatal(r, err)
	}
	rows, err := s.list(ctx)
	if err != nil || len(rows) != 1 || rows[0].Progress != 42 {
		t.Fatal(rows, err)
	}
	if err := s.finish(id, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	rows, err = s.list(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if _, ok, err := s.reserve(ctx, "/shows/A", "queued"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := s.update(ctx, id, "progress", string(data)); err == nil {
		t.Fatal("old worker updated completed reservation")
	}
}
func TestFailedLaunchDoesNotReleaseRunningWorker(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, err := s.reserve(ctx, "/show", "queued")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.update(ctx, id, "claim"); err != nil {
		t.Fatal(err)
	}
	r, err := s.update(ctx, id, "launch_failed", "response timed out")
	if err != nil || r.State != "running" || !r.Stop {
		t.Fatal(r, err)
	}
	if _, ok, err := s.reserve(ctx, "/show", "queued"); err != nil || ok {
		t.Fatal("running worker reservation released", ok, err)
	}
	if err := s.finish(id, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.reserve(ctx, "/show", "queued")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	r, err = s.update(ctx, id, "launch_failed", "spawn failed")
	if err != nil || r.State != "failed" {
		t.Fatal(r, err)
	}
	if _, err := s.update(ctx, id, "claim"); err == nil {
		t.Fatal("worker claimed failed launch")
	}
}

func TestRedisConcurrentReservations(t *testing.T) {
	s := testStore(t)
	var wg sync.WaitGroup
	accepted := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, ok, err := s.reserve(context.Background(), "/same", "queued")
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted <- id
			}
		}()
	}
	wg.Wait()
	close(accepted)
	if len(accepted) != 1 {
		t.Fatalf("accepted %d reservations", len(accepted))
	}
}
func TestRedisExpiredReservation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _ := s.reserve(ctx, "/same", "queued")
	raw, _ := s.client.HGet(ctx, s.key, id).Bytes()
	var r syncRecord
	json.Unmarshal(raw, &r)
	r.Expires = 1
	data, _ := json.Marshal(r)
	s.client.HSet(ctx, s.key, id, data)
	if _, ok, err := s.reserve(ctx, "/same", "queued"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := s.update(ctx, id, "claim"); err == nil {
		t.Fatal("expired worker claimed")
	}
}
func TestWorkerStopsThroughRedis(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	bin := t.TempDir()
	// Substitute rsync while exercising the real worker, process group and Redis.
	script := filepath.Join(bin, "rsync")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '1,234 42% 10.00kB/s 0:00:12'\nsleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	id, _, err := s.reserve(ctx, "/show", "queued")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- runSyncWorker(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{SyncWorker: id, DataPath: t.TempDir()}, s)
	}()
	deadline := time.After(5 * time.Second)
	for {
		rows, err := s.list(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Progress == 42 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not publish progress")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := s.stop(ctx, "/show"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("worker failed to stop")
	}
	raw, err := s.client.HGet(ctx, s.key, id).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var r syncRecord
	json.Unmarshal(raw, &r)
	if r.State != "cancelled" {
		t.Fatalf("state=%s", r.State)
	}
}
