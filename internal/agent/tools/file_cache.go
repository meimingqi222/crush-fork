package tools

import (
	"sync"
)

// FileCache manages in-memory file read snapshots per session.
// It is concurrent-safe and prevents shared memory mutation via slice cloning.
type FileCache struct {
	mu      sync.RWMutex
	cache   map[string]map[string][]string
	history map[string]map[string][][]string
	order   map[string][]string // sessionID -> list of filePaths in insertion order
}

// GlobalFileCache is the package-level cache instance shared between Read and Edit tools.
var GlobalFileCache = &FileCache{
	cache: make(map[string]map[string][]string),
	order: make(map[string][]string),
}

// Put stores a deep copy of file lines for a given session and path.
func (c *FileCache) Put(sessionID, filePath string, lines []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]map[string][]string)
	}
	if c.order == nil {
		c.order = make(map[string][]string)
	}
	if c.history == nil {
		c.history = make(map[string]map[string][][]string)
	}
	if _, ok := c.cache[sessionID]; !ok {
		c.cache[sessionID] = make(map[string][]string)
		c.order[sessionID] = []string{}
	}
	sessionCache := c.cache[sessionID]
	if _, ok := c.history[sessionID]; !ok {
		c.history[sessionID] = make(map[string][][]string)
	}
	_, exists := sessionCache[filePath]
	if !exists {
		// FIFO eviction if size exceeds 30
		if len(sessionCache) >= 30 {
			if len(c.order[sessionID]) > 0 {
				toEvict := c.order[sessionID][0]
				delete(sessionCache, toEvict)
				delete(c.history[sessionID], toEvict)
				c.order[sessionID] = c.order[sessionID][1:]
			}
		}
		c.order[sessionID] = append(c.order[sessionID], filePath)
	}

	copied := make([]string, len(lines))
	copy(copied, lines)
	if len(c.history[sessionID][filePath]) == 0 || !equalFileLines(c.history[sessionID][filePath][len(c.history[sessionID][filePath])-1], copied) {
		c.history[sessionID][filePath] = append(c.history[sessionID][filePath], copied)
		if len(c.history[sessionID][filePath]) > 4 {
			c.history[sessionID][filePath] = c.history[sessionID][filePath][1:]
		}
	}
	sessionCache[filePath] = copied
}

// Get retrieves a deep copy of file lines for a given session and path.
func (c *FileCache) Get(sessionID, filePath string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cache == nil {
		return nil, false
	}
	sessionCache, ok := c.cache[sessionID]
	if !ok {
		return nil, false
	}
	lines, ok := sessionCache[filePath]
	if !ok {
		return nil, false
	}
	copied := make([]string, len(lines))
	copy(copied, lines)
	return copied, true
}

func (c *FileCache) GetHistory(sessionID, filePath string) ([][]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.history == nil {
		return nil, false
	}
	sessionHistory, ok := c.history[sessionID]
	if !ok {
		return nil, false
	}
	snapshots, ok := sessionHistory[filePath]
	if !ok {
		return nil, false
	}
	result := make([][]string, len(snapshots))
	for i, snapshot := range snapshots {
		result[i] = append([]string(nil), snapshot...)
	}
	return result, true
}

func equalFileLines(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// Clear removes all cached snapshots for a given session.
func (c *FileCache) Clear(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache != nil {
		delete(c.cache, sessionID)
	}
	if c.order != nil {
		delete(c.order, sessionID)
	}
	if c.history != nil {
		delete(c.history, sessionID)
	}
}

// Delete removes a cached snapshot for a given session and path.
func (c *FileCache) Delete(sessionID, filePath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache != nil {
		sessionCache, ok := c.cache[sessionID]
		if ok {
			delete(sessionCache, filePath)
		}
	}
	if c.order != nil {
		orderList, ok := c.order[sessionID]
		if ok {
			for i, p := range orderList {
				if p == filePath {
					c.order[sessionID] = append(orderList[:i], orderList[i+1:]...)
					break
				}
			}
		}
	}
	if c.history != nil {
		if sessionHistory, ok := c.history[sessionID]; ok {
			delete(sessionHistory, filePath)
		}
	}
}
