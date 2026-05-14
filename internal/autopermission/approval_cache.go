package autopermission

import (
	"container/list"
	"sync"
)

const defaultApprovalCacheEntries = 512

type ApprovalCacheKey struct {
	SessionID    string
	ToolName     string
	Action       string
	WorkingDir   string
	Policy       ApprovalPolicy
	CapabilityID string
	Fingerprint  string
}

type approvalCache struct {
	mu      sync.Mutex
	max     int
	entries map[ApprovalCacheKey]*list.Element
	order   *list.List
}

type approvalCacheEntry struct {
	key ApprovalCacheKey
}

func newApprovalCache(maxEntries int) *approvalCache {
	if maxEntries <= 0 {
		maxEntries = defaultApprovalCacheEntries
	}
	return &approvalCache{
		max:     maxEntries,
		entries: make(map[ApprovalCacheKey]*list.Element),
		order:   list.New(),
	}
}

func (c *approvalCache) approved(keys []ApprovalCacheKey) bool {
	if c == nil || len(keys) == 0 {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		elem, ok := c.entries[key]
		if !ok {
			return false
		}
		c.order.MoveToFront(elem)
	}
	return true
}

func (c *approvalCache) approve(keys []ApprovalCacheKey) {
	if c == nil || len(keys) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		if key.Fingerprint == "" {
			continue
		}
		if elem, ok := c.entries[key]; ok {
			c.order.MoveToFront(elem)
			continue
		}
		elem := c.order.PushFront(approvalCacheEntry{key: key})
		c.entries[key] = elem
		for len(c.entries) > c.max {
			oldest := c.order.Back()
			if oldest == nil {
				break
			}
			entry := oldest.Value.(approvalCacheEntry)
			delete(c.entries, entry.key)
			c.order.Remove(oldest)
		}
	}
}

func (c *approvalCache) clearSession(sessionID string) {
	if c == nil || sessionID == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for elem := c.order.Back(); elem != nil; {
		prev := elem.Prev()
		entry := elem.Value.(approvalCacheEntry)
		if entry.key.SessionID == sessionID {
			delete(c.entries, entry.key)
			c.order.Remove(elem)
		}
		elem = prev
	}
}
