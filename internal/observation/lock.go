package observation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	lockRetryInterval = 10 * time.Millisecond
)

func acquireFileLock(path string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out acquiring lock %s; remove it if no observation process is active", path)
		}
		time.Sleep(lockRetryInterval)
	}
}
