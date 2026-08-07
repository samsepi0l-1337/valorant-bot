package valorantbot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTrackedSchedulerSupportsBoundedDrainThenBackgroundJoin(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	task := startTrackedScheduler(func() error {
		close(started)
		<-release
		return nil
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		unblock()
		t.Fatal("scheduler task did not start")
	}

	bounded, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := task.wait(bounded); !errors.Is(err, context.DeadlineExceeded) {
		unblock()
		t.Fatalf("bounded wait error = %v, want deadline exceeded", err)
	}

	dependencyClosed := make(chan struct{})
	go closeRuntimeAfterScheduler(task, func() { close(dependencyClosed) })
	select {
	case <-dependencyClosed:
		unblock()
		t.Fatal("dependency closed before scheduler task completed")
	case <-time.After(50 * time.Millisecond):
	}

	unblock()
	select {
	case <-dependencyClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("background join did not complete after scheduler task returned")
	}
}
