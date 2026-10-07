package status

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"antigravity-proxy/internal/quota"
)

func createHistorySymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires Windows privileges: %v", err)
		}
		t.Fatal(err)
	}
}

func TestHistoryRejectsFinalSymlinksBeforeCreatingLock(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target.jsonl")
			path := filepath.Join(dir, "linked.jsonl")
			sample := observation(observationTime(), 0.5)
			data := encodeHistory(t, []quota.Snapshot{sample})
			if exists {
				if err := os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			createHistorySymlink(t, target, path)
			c, err := Open(path, time.Minute, 1, func(context.Context) (quota.Snapshot, error) { return sample, nil })
			if err == nil {
				c.Close()
				t.Fatal("Open accepted a final history symlink")
			}
			if !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("symlink rejection = %v", err)
			}
			if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("rejected history symlink was changed: %v", err)
			}
			if _, err := os.Lstat(path + ".lock"); !os.IsNotExist(err) {
				t.Fatalf("rejected symlink created a separate lock: %v", err)
			}
			if exists {
				if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
					t.Fatalf("rejected symlink modified target history: %v", err)
				}
			} else if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("rejected dangling symlink created its target: %v", err)
			}
		})
	}
}

func TestHistoryRejectsCompanionLockSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	target := filepath.Join(dir, "unrelated-lock")
	if err := os.WriteFile(target, []byte("do not change"), 0600); err != nil {
		t.Fatal(err)
	}
	createHistorySymlink(t, target, path+".lock")
	if c, err := Open(path, time.Minute, 1, func(context.Context) (quota.Snapshot, error) { return quota.Snapshot{}, nil }); err == nil {
		c.Close()
		t.Fatal("Open accepted a companion lock symlink")
	} else if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("lock symlink rejection = %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "do not change" {
		t.Fatalf("lock symlink target changed: %v", err)
	}
}

func TestHistoryParentSymlinkAliasesSharePersistentLock(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "real")
	alias := filepath.Join(dir, "alias")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	createHistorySymlink(t, parent, alias)
	path := filepath.Join(alias, "history.jsonl")
	realPath := filepath.Join(parent, "history.jsonl")
	current := observation(observationTime(), 0.8)
	fetch := func(context.Context) (quota.Snapshot, error) { return current, nil }
	c := openRetainedCollector(t, path, 1, fetch)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	if c.path != filepath.Join(resolvedParent, "history.jsonl") {
		t.Fatalf("history path = %q; expected canonical parent %q", c.path, resolvedParent)
	}
	lockProbe(t, realPath, true)
	lockProbe(t, path, true)
	c.poll(context.Background())
	current = observation(observationTime().Add(time.Minute), 0.2)
	c.poll(context.Background())
	requireRetainedHistory(t, c, realPath, []quota.Snapshot{current})
	if info, err := os.Lstat(alias); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("compaction changed parent symlink: %v", err)
	}
	lockProbe(t, realPath, true)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openRetainedCollector(t, realPath, 1, fetch)
	requireRetainedHistory(t, reopened, path, []quota.Snapshot{current})
}

func TestHistoryReplacementRejectsSymlinkIntroducedAfterOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	target := filepath.Join(dir, "renamed-history")
	first := observation(observationTime(), 0.8)
	next := observation(observationTime().Add(time.Minute), 0.2)
	current := first
	c := openRetainedCollector(t, path, 1, func(context.Context) (quota.Snapshot, error) { return current, nil })
	c.poll(context.Background())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	staged := false
	c.storage.syncFile = func(temp *os.File) error {
		if err := temp.Sync(); err != nil {
			return err
		}
		staged = true
		// Mutate the path after the initial inspection and replacement Sync.
		// Reopen the old inode so the fixture also works on Windows, where an
		// open file normally cannot be renamed by an external process.
		if err := c.file.Close(); err != nil {
			return err
		}
		if err := os.Rename(path, target); err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_RDWR|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		c.file = file
		createHistorySymlink(t, target, path)
		return nil
	}
	current = next
	c.poll(context.Background())
	if !staged {
		t.Fatal("replacement did not reach its staging durability boundary")
	}
	if info := requireSnapshot(t, c, first); !strings.Contains(info.LastError, "symbolic link") {
		t.Fatalf("replacement accepted a final symlink: %+v", info)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement destroyed introduced symlink: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("replacement changed symlink target: %v", err)
	}
}

func createAbandonedHistoryCopy(t *testing.T, dir, prefix string) string {
	t.Helper()
	file, err := os.CreateTemp(dir, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("incomplete replacement"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func TestStartupRemovesOnlyOwnAbandonedHistoryCopiesUnderLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	sample := observation(observationTime(), 0.5)
	fetch := func(context.Context) (quota.Snapshot, error) { return sample, nil }
	owner := openRetainedCollector(t, path, 1, fetch)
	owner.poll(context.Background())
	other := openRetainedCollector(t, filepath.Join(dir, "other-history"), 1, fetch)
	other.poll(context.Background())
	otherCopy := createAbandonedHistoryCopy(t, dir, other.tempPrefix)
	legacy := createAbandonedHistoryCopy(t, dir, ".quota-history-")
	unrelated := createAbandonedHistoryCopy(t, dir, "unrelated-")
	matchingDirectory := filepath.Join(dir, owner.tempPrefix+"directory")
	if err := os.Mkdir(matchingDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	var ownCopies []string
	// More than one directory-read batch exercises cleanup without loading an
	// unbounded number of old crash files into memory at once.
	for range 130 {
		ownCopies = append(ownCopies, createAbandonedHistoryCopy(t, dir, owner.tempPrefix))
	}
	lockProbe(t, path, true)
	for _, name := range ownCopies {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("failed lock contender removed active-owner temporary file: %v", err)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openRetainedCollector(t, path, 1, fetch)
	requireRetainedHistory(t, reopened, path, []quota.Snapshot{sample})
	for _, name := range ownCopies {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("abandoned private copy survived startup: %s, %v", name, err)
		}
	}
	for _, name := range []string{otherCopy, legacy, unrelated, matchingDirectory} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("cleanup removed another history's or unrelated file: %s, %v", name, err)
		}
	}
	requireRetainedHistory(t, other, other.path, []quota.Snapshot{sample})
	lockProbe(t, other.path, true)
}

func TestHistoryCrashCopyProcessHelper(t *testing.T) {
	path := os.Getenv("ANTIGRAVITY_TEST_HISTORY_CRASH_PATH")
	if path == "" {
		return
	}
	c, err := Open(path, time.Minute, 2, func(context.Context) (quota.Snapshot, error) { panic("crash helper fetched quota") })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	temp, err := os.CreateTemp(filepath.Dir(c.path), c.tempPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer temp.Close()
	if _, err := temp.WriteString("incomplete replacement"); err != nil {
		t.Fatal(err)
	}
	fmt.Println(temp.Name())
	// Parent kills this process with the private file and companion lock open.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestStartupCleansPrivateCopyLeftByTerminatedProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	sample := observation(observationTime(), 0.5)
	data := encodeHistory(t, []quota.Snapshot{sample})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	other := openRetainedCollector(t, filepath.Join(dir, "other"), 1, func(context.Context) (quota.Snapshot, error) { return sample, nil })
	otherCopy := createAbandonedHistoryCopy(t, dir, other.tempPrefix)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHistoryCrashCopyProcessHelper$")
	command.Env = append(os.Environ(), "ANTIGRAVITY_TEST_HISTORY_CRASH_PATH="+path)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	var copyPath string
	select {
	case copyPath = <-ready:
		if !strings.HasPrefix(filepath.Base(copyPath), ".quota-history-") {
			_ = command.Wait()
			t.Fatalf("crash helper did not create a private copy: %q; %s", copyPath, stderr.String())
		}
	case <-ctx.Done():
		_ = command.Wait()
		t.Fatal("crash helper timed out")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if _, err := os.Stat(copyPath); err != nil {
		t.Fatalf("terminated process did not leave its private copy: %v", err)
	}
	c := openRetainedCollector(t, path, 2, func(context.Context) (quota.Snapshot, error) { return sample, nil })
	requireRetainedHistory(t, c, path, []quota.Snapshot{sample})
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("startup did not clean crashed owner's private copy: %v", err)
	}
	if _, err := os.Stat(otherCopy); err != nil {
		t.Fatalf("crash cleanup removed another history's private copy: %v", err)
	}
}
