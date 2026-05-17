package agent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
)

type AgentStatus string

const (
	AgentStatusRunning   AgentStatus = "running"
	AgentStatusIdle      AgentStatus = "idle"
	AgentStatusCompleted AgentStatus = "completed"
	AgentStatusAborted   AgentStatus = "aborted"
)

type AgentKind string

const (
	AgentKindMain AgentKind = "main"
	AgentKindSub  AgentKind = "sub"
)

type AgentRef struct {
	ID          string
	DisplayName string
	Kind        AgentKind
	ParentID    string
	Status      AgentStatus
	Agent       SessionAgent
	SessionID   string
	CreatedAt   time.Time
}

type RegistryListener func()

type listenerEntry struct {
	id uint64
	fn RegistryListener
}

type AgentRegistry struct {
	mu           sync.RWMutex
	refs         map[string]*AgentRef
	listeners    []listenerEntry
	nextListenID uint64
}

var (
	globalRegistry *AgentRegistry
	registryOnce   sync.Once
)

func GlobalAgentRegistry() *AgentRegistry {
	registryOnce.Do(func() {
		globalRegistry = &AgentRegistry{
			refs: make(map[string]*AgentRef),
		}
	})
	return globalRegistry
}

func (r *AgentRegistry) Register(ref AgentRef) *AgentRef {
	r.mu.Lock()
	snapshot := r.snapshotListeners()
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now()
	}
	entry := &ref
	r.refs[entry.ID] = entry
	r.mu.Unlock()
	r.fireListeners(snapshot)
	return entry
}

func (r *AgentRegistry) Unregister(id string) {
	r.mu.Lock()
	snapshot := r.snapshotListeners()
	delete(r.refs, id)
	r.mu.Unlock()
	r.fireListeners(snapshot)
}

func (r *AgentRegistry) Get(id string) (*AgentRef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.refs[id]
	if !ok {
		return nil, false
	}
	return ref, true
}

func (r *AgentRegistry) List() []*AgentRef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*AgentRef, 0, len(r.refs))
	for _, ref := range r.refs {
		result = append(result, ref)
	}
	return result
}

func (r *AgentRegistry) ListVisibleTo(id string) []*AgentRef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*AgentRef, 0, len(r.refs))
	for _, ref := range r.refs {
		if ref.ID == id {
			continue
		}
		if ref.Status != AgentStatusRunning && ref.Status != AgentStatusIdle {
			continue
		}
		result = append(result, ref)
	}
	return result
}

func (r *AgentRegistry) SetStatus(id string, status AgentStatus) {
	r.mu.Lock()
	snapshot := r.snapshotListeners()
	changed := false
	if ref, ok := r.refs[id]; ok && ref.Status != status {
		ref.Status = status
		changed = true
	}
	r.mu.Unlock()
	if changed {
		r.fireListeners(snapshot)
	}
}

func (r *AgentRegistry) OnChange(listener RegistryListener) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextListenID
	r.nextListenID++
	r.listeners = append(r.listeners, listenerEntry{id: id, fn: listener})
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, entry := range r.listeners {
			if entry.id == id {
				r.listeners = append(r.listeners[:i], r.listeners[i+1:]...)
				break
			}
		}
	}
}

func (r *AgentRegistry) snapshotListeners() []RegistryListener {
	snapshot := make([]RegistryListener, len(r.listeners))
	for i, entry := range r.listeners {
		snapshot[i] = entry.fn
	}
	return snapshot
}

func (r *AgentRegistry) fireListeners(listeners []RegistryListener) {
	for _, fn := range listeners {
		fn()
	}
}

func (r *AgentRegistry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = make(map[string]*AgentRef)
	r.listeners = nil
	r.nextListenID = 0
}

func (r *AgentRegistry) AsIrcRegistry() *ircRegistryAdapter {
	return &ircRegistryAdapter{registry: r}
}

type ircRegistryAdapter struct {
	registry *AgentRegistry
}

func (a *ircRegistryAdapter) Get(id string) (agenttools.IrcPeerInfo, bool) {
	ref, ok := a.registry.Get(id)
	if !ok {
		return agenttools.IrcPeerInfo{}, false
	}
	return agenttools.IrcPeerInfo{
		ID:          ref.ID,
		DisplayName: ref.DisplayName,
		Kind:        string(ref.Kind),
		Status:      string(ref.Status),
		ParentID:    ref.ParentID,
	}, true
}

func (a *ircRegistryAdapter) ListVisibleTo(id string) []agenttools.IrcPeerInfo {
	refs := a.registry.ListVisibleTo(id)
	peers := make([]agenttools.IrcPeerInfo, 0, len(refs))
	for _, ref := range refs {
		peers = append(peers, agenttools.IrcPeerInfo{
			ID:          ref.ID,
			DisplayName: ref.DisplayName,
			Kind:        string(ref.Kind),
			Status:      string(ref.Status),
			ParentID:    ref.ParentID,
		})
	}
	return peers
}

func renderIrcPeerRoster(registry *AgentRegistry, selfID string) string {
	peers := registry.ListVisibleTo(selfID)
	if len(peers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<irc_peers>\n")
	b.WriteString("You are part of a multi-agent team. The following agents are currently live and reachable via the `irc` tool.\n")
	b.WriteString("Use `irc` with op=send to communicate with any peer. Use op=list to check current availability.\n\n")
	for _, peer := range peers {
		b.WriteString(fmt.Sprintf("- `%s` — %s (%s, %s)\n", peer.ID, peer.DisplayName, peer.Kind, peer.Status))
	}
	b.WriteString("</irc_peers>")
	return b.String()
}
