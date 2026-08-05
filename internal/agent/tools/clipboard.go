package tools

import "sync"

// ClipboardRegister stores lines captured by a CUT operation for later PASTE.
type ClipboardRegister struct {
	Lines []string
}

// Clipboard manages session-level clipboard registers for CUT/PASTE operations
// across multiple edit calls. Named registers persist for the session lifetime;
// the anonymous register is batch-local (single edit call).
type Clipboard struct {
	mu      sync.RWMutex
	named   map[string]map[string]*ClipboardRegister // sessionID -> registerName -> register
	anon    map[string]*ClipboardRegister            // sessionID -> anonymous register
}

// GlobalClipboard is the package-level clipboard instance shared between edit calls.
var GlobalClipboard = &Clipboard{
	named: make(map[string]map[string]*ClipboardRegister),
	anon:  make(map[string]*ClipboardRegister),
}

// PutNamed stores lines into a named register for a given session.
func (c *Clipboard) PutNamed(sessionID, name string, lines []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.named == nil {
		c.named = make(map[string]map[string]*ClipboardRegister)
	}
	if _, ok := c.named[sessionID]; !ok {
		c.named[sessionID] = make(map[string]*ClipboardRegister)
	}
	copied := make([]string, len(lines))
	copy(copied, lines)
	c.named[sessionID][name] = &ClipboardRegister{Lines: copied}
}

// GetNamed retrieves lines from a named register for a given session.
func (c *Clipboard) GetNamed(sessionID, name string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.named == nil {
		return nil, false
	}
	sessionRegisters, ok := c.named[sessionID]
	if !ok {
		return nil, false
	}
	reg, ok := sessionRegisters[name]
	if !ok {
		return nil, false
	}
	copied := make([]string, len(reg.Lines))
	copy(copied, reg.Lines)
	return copied, true
}

// PutAnonymous stores lines into the anonymous (batch-local) register for a given session.
func (c *Clipboard) PutAnonymous(sessionID string, lines []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.anon == nil {
		c.anon = make(map[string]*ClipboardRegister)
	}
	copied := make([]string, len(lines))
	copy(copied, lines)
	c.anon[sessionID] = &ClipboardRegister{Lines: copied}
}

// GetAnonymous retrieves lines from the anonymous register for a given session.
func (c *Clipboard) GetAnonymous(sessionID string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.anon == nil {
		return nil, false
	}
	reg, ok := c.anon[sessionID]
	if !ok {
		return nil, false
	}
	copied := make([]string, len(reg.Lines))
	copy(copied, reg.Lines)
	return copied, true
}

// ClearAnonymous clears the anonymous register for a given session (called at end of batch).
func (c *Clipboard) ClearAnonymous(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.anon, sessionID)
}

// Clear removes all clipboard registers for a given session.
func (c *Clipboard) Clear(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.named, sessionID)
	delete(c.anon, sessionID)
}
