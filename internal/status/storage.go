package status

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"antigravity-proxy/internal/quota"
)

// historyStorage separates the three durability boundaries of replacement.
// Each collector owns its operations so fault injection cannot affect another
// collector. The companion lock is never replaced or removed.
type historyStorage struct {
	syncFile func(*os.File) error
	replace  func(string, string) error
	syncDir  func(string) error
}

func defaultHistoryStorage() historyStorage {
	return historyStorage{syncFile: (*os.File).Sync, replace: replaceHistoryFile, syncDir: syncHistoryDirectory}
}

func inspectHistoryPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect quota history: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("quota history path must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return errors.New("quota history path must be a regular file")
	}
	return nil
}

// The canonical path digest prevents cleanup for one history from matching
// another history's private files, including histories in the same directory.
func historyTempPrefix(path string) string {
	return fmt.Sprintf(".quota-history-%x-", sha256.Sum256([]byte(path)))
}

// cleanHistoryTemps runs only after acquiring the persistent companion lock.
// A failed contender cannot remove files belonging to the active owner. Legacy
// anonymous .quota-history-* files cannot be attributed safely and are left alone.
func (c *Collector) cleanHistoryTemps() error {
	dir := filepath.Dir(c.path)
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open quota history directory for cleanup: %w", err)
	}
	defer directory.Close()
	removed := false
	for {
		entries, readErr := directory.ReadDir(64)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("inspect abandoned quota history files: %w", readErr)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), c.tempPrefix) || !entry.Type().IsRegular() {
				continue
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("remove abandoned quota history file: %w", err)
			}
			removed = true
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if removed {
		if err := c.storage.syncDir(dir); err != nil {
			return fmt.Errorf("sync quota history cleanup: %w", err)
		}
	}
	return nil
}

// loadHistory validates every complete record, including records outside the
// retained suffix. A ring bounds startup memory even for an oversized old file.
func (c *Collector) loadHistory(file *os.File) error {
	reader := bufio.NewReader(file)
	var offset int64
	oldest := 0
	compact := false
	for line := 1; ; line++ {
		record, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read quota history: %w", readErr)
		}
		if len(record) != 0 {
			if record[len(record)-1] != '\n' {
				if err := recoverHistoryTail(file, offset); err != nil {
					return fmt.Errorf("recover quota history line %d: %w", line, err)
				}
				slog.Warn("recovered incomplete quota history record", "path", c.path, "line", line, "discarded_bytes", len(record))
				break
			}
			var sample quota.Snapshot
			if err := json.Unmarshal(record, &sample); err != nil {
				return fmt.Errorf("quota history line %d: %w", line, err)
			}
			if err := validateSnapshot(sample); err != nil {
				return fmt.Errorf("quota history line %d: %w", line, err)
			}
			if len(c.samples) < c.maxSamples {
				c.samples = append(c.samples, sample)
			} else {
				c.samples[oldest] = sample
				oldest = (oldest + 1) % c.maxSamples
				compact = true
			}
			offset += int64(len(record))
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if compact {
		// Rotate the retained ring in place into append order before building the
		// observation-time index. No discarded snapshots survive in the cache.
		reverseSamples(c.samples[:oldest])
		reverseSamples(c.samples[oldest:])
		reverseSamples(c.samples)
		if err := c.replaceHistory(c.samples, nil); err != nil {
			return err
		}
	}
	return nil
}

func reverseSamples(samples []quota.Snapshot) {
	for left, right := 0, len(samples)-1; left < right; left, right = left+1, right-1 {
		samples[left], samples[right] = samples[right], samples[left]
	}
}

func recoverHistoryTail(file historyFile, offset int64) error {
	if err := file.Truncate(offset); err != nil {
		return fmt.Errorf("truncate incomplete record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync recovered history: %w", err)
	}
	return nil
}

// replaceHistory commits a complete retained suffix, optionally including the
// next append, by syncing a private same-directory file, replacing the history
// path, and syncing the directory. The caller publishes memory only on success.
// Failures before replacement leave the old history untouched. Failures after
// replacement fault the collector: durability is uncertain and no further writes
// are safe. Its companion lock remains held in either case until Close.
func (c *Collector) replaceHistory(samples []quota.Snapshot, next *quota.Snapshot) error {
	if err := inspectHistoryPath(c.path); err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	temp, err := os.CreateTemp(dir, c.tempPrefix)
	if err != nil {
		return fmt.Errorf("compact quota history: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	defer temp.Close()
	encoder := json.NewEncoder(temp)
	for _, sample := range samples {
		if err := encoder.Encode(sample); err != nil {
			return fmt.Errorf("write compacted quota history: %w", err)
		}
	}
	if next != nil {
		if err := encoder.Encode(next); err != nil {
			return fmt.Errorf("write compacted quota history: %w", err)
		}
	}
	if err := c.storage.syncFile(temp); err != nil {
		return fmt.Errorf("sync compacted quota history: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close compacted quota history: %w", err)
	}
	if err := inspectHistoryPath(c.path); err != nil {
		return err
	}
	// Windows requires both history handles closed before replacing either
	// path. The separate lock still excludes all cooperating writers.
	if err := c.file.Close(); err != nil {
		c.file = nil
		c.storeFault = fmt.Errorf("close quota history before compaction: %w", err)
		return c.storeFault
	}
	c.file = nil
	replaceErr := c.storage.replace(name, c.path)
	var durabilityErr error
	if replaceErr == nil {
		durabilityErr = c.storage.syncDir(dir)
	}
	file, openErr := os.OpenFile(c.path, os.O_RDWR, 0600)
	if openErr == nil {
		c.file = file
	}
	if durabilityErr != nil || openErr != nil {
		c.storeFault = fmt.Errorf("compact quota history storage fault: %w", errors.Join(replaceErr, durabilityErr, openErr))
		return c.storeFault
	}
	if replaceErr != nil {
		return fmt.Errorf("replace quota history: %w", replaceErr)
	}
	return nil
}

// POSIX fcntl locks belong to the process, not the descriptor: opening and
// closing a second handle to the same inode can release the owner's lock.
// Serialize acquisition and reject same-file contenders before opening them.
// File identity also covers hard-linked companion paths.
var historyLocks struct {
	sync.Mutex
	owners []*historyLock
}

type historyLock struct {
	file     *os.File
	info     os.FileInfo
	closed   bool
	closeErr error
}

func (lock *historyLock) Close() error {
	historyLocks.Lock()
	defer historyLocks.Unlock()
	if lock.closed {
		return lock.closeErr
	}
	lock.closeErr = lock.file.Close()
	lock.closed = true
	for index, owner := range historyLocks.owners {
		if owner == lock {
			copy(historyLocks.owners[index:], historyLocks.owners[index+1:])
			historyLocks.owners[len(historyLocks.owners)-1] = nil
			historyLocks.owners = historyLocks.owners[:len(historyLocks.owners)-1]
			break
		}
	}
	return lock.closeErr
}

func lockHistory(path string) (*historyLock, error) {
	historyLocks.Lock()
	defer historyLocks.Unlock()
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("quota history lock must not be a symbolic link")
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("quota history lock must be a regular file")
		}
		for _, owner := range historyLocks.owners {
			if os.SameFile(info, owner.info) {
				return nil, errors.New("lock quota history (another collector owns it)")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect quota history lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open quota history lock: %w", err)
	}
	fail := func(err error) (*historyLock, error) {
		_ = file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fail(errors.New("quota history lock must be a regular file"))
	}
	if err := file.Chmod(0600); err != nil {
		return fail(fmt.Errorf("protect quota history lock: %w", err))
	}
	if err := lockHistoryFile(file); err != nil {
		return fail(fmt.Errorf("lock quota history (another process may own it): %w", err))
	}
	lock := &historyLock{file: file, info: info}
	historyLocks.owners = append(historyLocks.owners, lock)
	return lock, nil
}
