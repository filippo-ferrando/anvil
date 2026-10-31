//go:build linux

package vm

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
)

// memoryEnv set to "uffd" serves clones' memory through a page handler in anvild instead of
// mapping the warm memory file: faster first steps for large guests, but no shared pages.
const memoryEnv = "ANVIL_EPH_MEMORY"

// warmWorkingSet is the file in a warm dir listing the memory pages a clone touched first.
const warmWorkingSet = "workingset"

// workingSetWindow is how long after its restore the first uffd clone records the pages it touches.
var workingSetWindow = 30 * time.Second

// cloneMemory is the memory backend clone c loads with: the warm memory file mapped
// private, or a page handler that prefetches the warm snapshot's working set.
func (b *FCBackend) cloneMemory(c *fcClone, dir string) (firecracker.MemoryBackend, error) {
	if os.Getenv(memoryEnv) != "uffd" {
		return firecracker.MemoryBackend{BackendType: "File", BackendPath: c.paths.InVMM(filepath.Join(c.paths.root(), warmMem))}, nil
	}
	wsPath := filepath.Join(dir, warmWorkingSet)
	prefetch, err := firecracker.ReadWorkingSet(wsPath)
	if err != nil {
		log.Printf("vm: %v, restoring %s without a prefetch", err, c.id)
		prefetch = nil
	}
	var record time.Duration
	if prefetch == nil {
		record = workingSetWindow // the first clone records it for the next ones
	}
	sock := filepath.Join(c.paths.root(), "uffd.sock")
	h, err := firecracker.ListenPages(sock, filepath.Join(dir, warmMem), prefetch, record)
	if err != nil {
		return firecracker.MemoryBackend{}, err
	}
	c.pages = h
	if record > 0 {
		c.saveWorkingSet = sync.OnceFunc(func() { saveWorkingSet(h, wsPath) })
		time.AfterFunc(record, c.saveWorkingSet)
	}
	return firecracker.MemoryBackend{BackendType: "Uffd", BackendPath: c.paths.InVMM(sock)}, nil
}

// saveWorkingSet writes what h recorded, unless another clone already did.
func saveWorkingSet(h *firecracker.PageHandler, path string) {
	if _, err := os.Stat(path); err == nil {
		return
	}
	pages := h.WorkingSet()
	if len(pages) == 0 {
		return
	}
	if err := firecracker.WriteWorkingSet(path, pages); err != nil {
		log.Printf("vm: saving the working set %s: %v", path, err)
	}
}
