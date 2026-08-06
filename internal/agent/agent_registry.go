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

// agentSnapshot is a value copy of the AgentRef fields needed to describe a
// peer. Taking a copy while the registry lock is held is mandatory for any
// field SetStatus mutates: refs are stored by pointer, so reading Status off
// a *AgentRef returned by Get/List/ListVisibleTo races with a concurrent
// SetStatus write. Agent is set once at Register and never reassigned, so
// carrying it out of the lock is safe -- and necessary, because IsBusy()
// must not be called with r.mu held.
type agentSnapshot struct {
	ID          string
	DisplayName string
	Kind        AgentKind
	ParentID    string
	Status      AgentStatus
	Agent       SessionAgent
}

func snapshotOf(ref *AgentRef) agentSnapshot {
	return agentSnapshot{
		ID:          ref.ID,
		DisplayName: ref.DisplayName,
		Kind:        ref.Kind,
		ParentID:    ref.ParentID,
		Status:      ref.Status,
		Agent:       ref.Agent,
	}
}

// snapshot returns a lock-safe copy of id's peer fields.
func (r *AgentRegistry) snapshot(id string) (agentSnapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.refs[id]
	if !ok {
		return agentSnapshot{}, false
	}
	return snapshotOf(ref), true
}

// snapshotVisibleTo returns lock-safe copies of the peers visible to id,
// mirroring ListVisibleTo's filter. Prefer this over ListVisibleTo whenever
// the caller reads Status, which ListVisibleTo cannot expose safely.
func (r *AgentRegistry) snapshotVisibleTo(id string) []agentSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]agentSnapshot, 0, len(r.refs))
	for _, ref := range r.refs {
		if ref.ID == id {
			continue
		}
		if ref.Status != AgentStatusRunning && ref.Status != AgentStatusIdle {
			continue
		}
		result = append(result, snapshotOf(ref))
	}
	return result
}

// EffectiveStatus returns id's externally observable status, deriving Idle
// vs Running from the attached SessionAgent's busy state when one is
// present. This exists because the registry's own Idle/Running writes can go
// stale for long-lived agents -- most notably the primary agent, which is
// registered once at startup (see coordinator.go's registration of
// "0-Main") and has no code path that ever calls SetStatus on it again, so
// without this it reports Idle forever regardless of whether a turn is
// running. Deriving at query time is preferred over adding a second write
// path (e.g. SetStatus at run start/end) that could drift out of sync with
// the SessionAgent's actual run state.
//
// Terminal statuses (Aborted, Completed) are returned unchanged: a finished
// agent has no meaningful "busy" reading, and this must not resurrect it.
func (r *AgentRegistry) EffectiveStatus(id string) (AgentStatus, bool) {
	snap, ok := r.snapshot(id)
	if !ok {
		return "", false
	}
	// IsBusy() runs after the registry lock is released: it may touch the
	// SessionAgent's own locking/state, and holding r.mu across a call into
	// external code risks a lock-ordering deadlock if that code ever calls
	// back into the registry (e.g. via OnChange listeners).
	return effectiveStatus(snap.Status, snap.Agent), true
}

// effectiveStatus derives the observable Idle/Running status for an agent
// given its registry-stored status and (possibly nil) live SessionAgent
// instance. See EffectiveStatus for why this derivation exists.
func effectiveStatus(status AgentStatus, agent SessionAgent) AgentStatus {
	if agent == nil {
		return status
	}
	switch status {
	case AgentStatusIdle, AgentStatusRunning:
		if agent.IsBusy() {
			return AgentStatusRunning
		}
		return AgentStatusIdle
	default:
		return status
	}
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

// peerInfo projects a snapshot into the model/tool-facing peer shape,
// deriving the observable status outside the registry lock.
func peerInfo(snap agentSnapshot) agenttools.IrcPeerInfo {
	return agenttools.IrcPeerInfo{
		ID:          snap.ID,
		DisplayName: snap.DisplayName,
		Kind:        string(snap.Kind),
		Status:      string(effectiveStatus(snap.Status, snap.Agent)),
		ParentID:    snap.ParentID,
	}
}

func (a *ircRegistryAdapter) Get(id string) (agenttools.IrcPeerInfo, bool) {
	snap, ok := a.registry.snapshot(id)
	if !ok {
		return agenttools.IrcPeerInfo{}, false
	}
	return peerInfo(snap), true
}

func (a *ircRegistryAdapter) ListVisibleTo(id string) []agenttools.IrcPeerInfo {
	snaps := a.registry.snapshotVisibleTo(id)
	peers := make([]agenttools.IrcPeerInfo, 0, len(snaps))
	for _, snap := range snaps {
		peers = append(peers, peerInfo(snap))
	}
	return peers
}

func renderIrcPeerRoster(registry *AgentRegistry, selfID string) string {
	snaps := registry.snapshotVisibleTo(selfID)
	if len(snaps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<irc_peers>\n")
	b.WriteString("You are part of a multi-agent team. The following agents are currently live and reachable via the `irc` tool.\n")
	b.WriteString("Use `irc` with op=send to communicate with any peer. Use op=list to check current availability.\n\n")
	for _, snap := range snaps {
		peer := peerInfo(snap)
		fmt.Fprintf(&b, "- `%s` — %s (%s, %s)\n", peer.ID, peer.DisplayName, peer.Kind, peer.Status)
	}
	b.WriteString("</irc_peers>")
	return b.String()
}
