package valorantbot

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireInstanceLock prevents two bot processes from sharing one Discord token/DB.
// Hold the returned file open until shutdown; closing releases the lock.
func acquireInstanceLock(databasePath string) (*os.File, error) {
	dir := filepath.Dir(databasePath)
	if dir == "" || dir == "." {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lock dir: %w", err)
	}
	path := filepath.Join(dir, "bot.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another valorant-bot is already running (lock %s). Stop it first: pkill -f valorant-bot", path)
	}
	_, _ = f.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	return f, nil
}
