package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

func runSyncWorker(parent context.Context, logger *slog.Logger, config Config, store *redisSyncStore) error {
	notifier := newMQTTSyncNotifier(config, logger)
	defer notifier.Close()
	return runSyncWorkerWithNotifications(parent, logger, config, store, notifier.Notify)
}

func runSyncWorkerWithNotifications(parent context.Context, logger *slog.Logger, config Config, store *redisSyncStore, notify syncNotifyFunc) error {
	record, err := store.update(parent, config.SyncWorker, "claim")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	current := &Sync{Path: record.Path, Context: ctx, Cancel: cancel}
	storage := &syncStorage{}
	if record.Stop {
		cancel()
	}
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				storage.Lock()
				progress := SyncResult{Path: current.Path, Progress: current.Progress, Speed: current.Speed, Downloaded: current.Downloaded, TimeLeft: current.TimeLeft}
				storage.Unlock()
				data, _ := json.Marshal(progress)
				updateCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				r, e := store.update(updateCtx, record.ID, "progress", string(data))
				stop()
				if e != nil {
					cancel()
					done <- e
					return
				}
				if r.Stop {
					cancel()
					done <- nil
					return
				}
			}
		}
	}()
	var runErr error
	if ctx.Err() == nil {
		notify("start", record.ID, record.Path, "running", nil)
		if ctx.Err() == nil {
			runErr = startSync(logger, config, storage, current)
		} else {
			runErr = ctx.Err()
		}
	} else {
		runErr = ctx.Err()
	}
	cancel()
	monitorErr := <-done
	state := "completed"
	if runErr != nil {
		state = "failed"
	}
	if errors.Is(runErr, context.Canceled) {
		state = "cancelled"
	}
	if monitorErr != nil {
		state = "failed"
		runErr = fmt.Errorf("worker lost Redis connection: %w", monitorErr)
	}
	// Persist the last fields even if rsync completed between heartbeat ticks.
	storage.Lock()
	progress := SyncResult{Path: current.Path, Progress: current.Progress, Speed: current.Speed, Downloaded: current.Downloaded, TimeLeft: current.TimeLeft}
	storage.Unlock()
	data, _ := json.Marshal(progress)
	finalCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	_, updateErr := store.update(finalCtx, record.ID, "progress", string(data))
	stop()
	finishErr := store.finish(record.ID, state, runErr)
	if state == "cancelled" {
		runErr = nil
	}
	resultErr := errors.Join(runErr, updateErr, finishErr)
	event := "complete"
	if state == "cancelled" {
		event = "cancelled"
	}
	if resultErr != nil {
		event = "error"
		state = "failed"
	}
	notify(event, record.ID, record.Path, state, resultErr)
	return resultErr
}

func removeWithRedis(ctx context.Context, config Config, store *redisSyncStore, path string) (bool, error) {
	id, ok, err := store.reserve(ctx, path, "removing")
	if err != nil || !ok {
		return false, err
	}
	// Keep the reservation alive during a potentially long recursive deletion.
	done := make(chan struct{})
	heartbeat := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				heartbeat <- nil
				return
			case <-ticker.C:
				updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := store.update(updateCtx, id, "heartbeat")
				cancel()
				if err != nil {
					heartbeat <- err
					return
				}
			}
		}
	}()
	err = os.RemoveAll(filepath.Join(config.DataPath, path))
	close(done)
	monitorErr := <-heartbeat
	state := "completed"
	if err != nil {
		state = "failed"
	}
	finishErr := store.finish(id, state, err)
	return err == nil, errors.Join(err, monitorErr, finishErr)
}
