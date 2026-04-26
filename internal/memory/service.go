package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultLimit              = 20
	maxLimit                  = 100
	maxMemoryFiles            = 200
	indexFilename             = "MEMORY.md"
	consolidatedAtFilename    = ".last_consolidated_at"
	consolidationLockFilename = ".consolidation.lock"
	sessionScope              = "session"

	// sessionTTL is the maximum age for session-scoped memories.
	// They are excluded from listings and removed on access after this age.
	sessionTTL = 7 * 24 * time.Hour

	// defaultImportance is applied when no importance is specified.
	defaultImportance = 0.5
	// minImportance and maxImportance bound the importance value.
	minImportance = 0.0
	maxImportance = 1.0

	// accessFlushBatch controls how many reads must occur before
	// the access counters are persisted back to disk. This avoids
	// a random write on every Get call while still keeping the
	// recency/frequency signal roughly accurate.
	accessFlushBatch = 5
)

var ErrNotFound = errors.New("memory key not found")

type Entry struct {
	Key            string   `json:"key" yaml:"key"`
	Value          string   `json:"value" yaml:"value"`
	Description    string   `json:"description,omitempty" yaml:"description,omitempty"`
	Scope          string   `json:"scope,omitempty" yaml:"scope,omitempty"`
	Category       string   `json:"category,omitempty" yaml:"category,omitempty"`
	Type           string   `json:"type,omitempty" yaml:"type,omitempty"`
	Tags           []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Importance     float64  `json:"importance,omitempty" yaml:"importance,omitempty"`
	AccessCount    int64    `json:"access_count,omitempty" yaml:"access_count,omitempty"`
	LastAccessedAt int64    `json:"last_accessed_at,omitempty" yaml:"last_accessed_at,omitempty"`
	UpdatedAt      int64    `json:"updated_at" yaml:"updated_at"`
}

type StoreParams struct {
	Key         string
	Value       string
	Description string
	Scope       string
	Category    string
	Type        string
	Tags        []string
	Importance  float64
}

type SearchParams struct {
	Query    string
	Scope    string
	Category string
	Type     string
	Tags     []string
	Limit    int
}

type ListParams struct {
	Scope    string
	Category string
	Type     string
	Tags     []string
	Limit    int
}

type Service interface {
	Store(context.Context, StoreParams) error
	Get(context.Context, string) (Entry, error)
	Delete(context.Context, string) error
	Search(context.Context, SearchParams) ([]Entry, error)
	List(context.Context, ListParams) ([]Entry, error)
	ReadIndex() (string, error)
	ListMemoryFiles() ([]MemoryFileInfo, error)
	ReadMemoryFileBody(string) (string, error)
	ReadLastConsolidatedAt() (time.Time, error)
	WriteLastConsolidatedAt(time.Time) error
	TryAcquireConsolidationLock(string, time.Duration) (bool, error)
	ReleaseConsolidationLock(string) error
}

// pendingAccess tracks unflushed access-counter increments for a memory
// key so that Store can merge them instead of overwriting with stale disk
// values.
type pendingAccess struct {
	count  int64
	lastAt int64
}

type service struct {
	memoryDir     string
	indexPath     string
	mu            sync.Mutex
	pendingAccess map[string]pendingAccess
}

type memoryFrontmatter struct {
	Key            string   `yaml:"key"`
	Description    string   `yaml:"description"`
	Scope          string   `yaml:"scope,omitempty"`
	Category       string   `yaml:"category,omitempty"`
	Type           string   `yaml:"type,omitempty"`
	Tags           []string `yaml:"tags,omitempty"`
	Importance     float64  `yaml:"importance,omitempty"`
	AccessCount    int64    `yaml:"access_count,omitempty"`
	LastAccessedAt int64    `yaml:"last_accessed_at,omitempty"`
}

func NewService(dataDir string) (Service, error) {
	root := strings.TrimSpace(dataDir)
	if root == "" {
		return nil, fmt.Errorf("data directory is required")
	}

	memoryDir := filepath.Join(root, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating memory directory: %w", err)
	}

	s := &service{
		memoryDir:     memoryDir,
		indexPath:     filepath.Join(memoryDir, indexFilename),
		pendingAccess: make(map[string]pendingAccess),
	}
	if err := s.ensureIndexFile(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *service) Store(ctx context.Context, params StoreParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	normalizedKey := strings.TrimSpace(params.Key)
	if normalizedKey == "" {
		return fmt.Errorf("key is required")
	}
	if strings.TrimSpace(params.Value) == "" {
		return fmt.Errorf("value is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	description := strings.TrimSpace(params.Description)
	if description == "" {
		description = truncateForDescription(params.Value)
	}

	importance := clampImportance(params.Importance)
	if importance == 0 {
		importance = defaultImportanceForType(params.Type)
	}

	// Preserve access counters across updates so importance is not lost.
	// Merge any unflushed increments from Get calls that have not yet been
	// persisted to disk.
	var existingCount int64
	var existingLastAccessed int64
	if existing, err := s.readEntryLocked(normalizedKey); err == nil {
		existingCount = existing.AccessCount
		existingLastAccessed = existing.LastAccessedAt
	}
	if pending, ok := s.pendingAccess[normalizedKey]; ok {
		if pending.count > existingCount {
			existingCount = pending.count
			existingLastAccessed = pending.lastAt
		}
		delete(s.pendingAccess, normalizedKey)
	}

	fm := memoryFrontmatter{
		Key:            normalizedKey,
		Description:    description,
		Scope:          strings.TrimSpace(params.Scope),
		Category:       strings.TrimSpace(params.Category),
		Type:           strings.TrimSpace(params.Type),
		Tags:           normalizeTags(params.Tags),
		Importance:     importance,
		AccessCount:    existingCount,
		LastAccessedAt: existingLastAccessed,
	}

	filePath := s.entryFilePath(normalizedKey)
	content := buildMemoryFileContent(fm, params.Value)
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing memory file: %w", err)
	}

	if err := s.rebuildIndexLocked(); err != nil {
		return fmt.Errorf("rebuilding index: %w", err)
	}
	return nil
}

func (s *service) Get(ctx context.Context, key string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}

	normalizedKey := strings.TrimSpace(key)
	if normalizedKey == "" {
		return Entry{}, fmt.Errorf("key is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, err := s.readEntryLocked(normalizedKey)
	if err != nil {
		return Entry{}, err
	}

	// Merge any pending unflushed increments so consecutive Get calls
	// see a monotonically increasing counter instead of reading the
	// stale disk value.
	if pending, ok := s.pendingAccess[normalizedKey]; ok && pending.count > entry.AccessCount {
		entry.AccessCount = pending.count
		entry.LastAccessedAt = pending.lastAt
	}

	// Bump access counters on read.
	entry.AccessCount++
	entry.LastAccessedAt = time.Now().UnixNano()

	// Track the unflushed increment so Store can merge it later.
	s.pendingAccess[normalizedKey] = pendingAccess{
		count:  entry.AccessCount,
		lastAt: entry.LastAccessedAt,
	}

	// Persist counters back to disk only every accessFlushBatch reads
	// to avoid a random write on every Get call.
	if entry.AccessCount%accessFlushBatch == 0 {
		if writeErr := s.writeAccessMetaLocked(entry); writeErr != nil {
			// Best-effort: do not fail the read; leave pending intact
			// for the next Store or flush attempt.
			_ = writeErr
		} else {
			delete(s.pendingAccess, normalizedKey)
		}
	}
	return entry, nil
}

func (s *service) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	normalizedKey := strings.TrimSpace(key)
	if normalizedKey == "" {
		return fmt.Errorf("key is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filePath := s.entryFilePath(normalizedKey)
	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("checking memory file: %w", err)
	}

	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("deleting memory file: %w", err)
	}

	delete(s.pendingAccess, normalizedKey)
	return s.rebuildIndexLocked()
}

func (s *service) Search(ctx context.Context, params SearchParams) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	queryTokens := tokenizeSearchText(params.Query)
	if len(queryTokens) == 0 {
		return nil, fmt.Errorf("query is required")
	}
	filters := entryFilters{
		Scope:    strings.TrimSpace(params.Scope),
		Category: strings.TrimSpace(params.Category),
		Type:     strings.TrimSpace(params.Type),
		Tags:     normalizeTags(params.Tags),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.loadAllEntriesLocked()
	if err != nil {
		return nil, err
	}

	type scoredEntry struct {
		entry memoryEntry
		score float64
	}
	matches := make([]scoredEntry, 0, len(entries))
	for _, entry := range entries {
		if !shouldIncludeEntry(entry, filters.Scope) {
			continue
		}
		if !matchesEntryFilters(entry, filters) {
			continue
		}
		score := scoreKeywordMatch(entry, queryTokens)
		if score <= 0 {
			continue
		}
		matches = append(matches, scoredEntry{entry: memoryEntry(entry), score: score})
	}

	now := time.Now()
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		ei, ej := Entry(matches[i].entry), Entry(matches[j].entry)
		si, sj := entryScore(ei, now), entryScore(ej, now)
		if si != sj {
			return si > sj
		}
		if ei.UpdatedAt != ej.UpdatedAt {
			return ei.UpdatedAt > ej.UpdatedAt
		}
		return ei.Key < ej.Key
	})

	results := make([]Entry, 0, len(matches))
	for _, match := range matches {
		results = append(results, Entry(match.entry))
	}
	return applyLimit(results, params.Limit), nil
}

func (s *service) List(ctx context.Context, params ListParams) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filters := entryFilters{
		Scope:    strings.TrimSpace(params.Scope),
		Category: strings.TrimSpace(params.Category),
		Type:     strings.TrimSpace(params.Type),
		Tags:     normalizeTags(params.Tags),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.loadAllEntriesLocked()
	if err != nil {
		return nil, err
	}

	results := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if !shouldIncludeEntry(entry, filters.Scope) {
			continue
		}
		if !matchesEntryFilters(entry, filters) {
			continue
		}
		results = append(results, entry)
	}
	sortEntries(results)
	return applyLimit(results, params.Limit), nil
}

func (s *service) ensureIndexFile() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(s.indexPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking memory index: %w", err)
	}

	return os.WriteFile(s.indexPath, []byte("# Memory Index\n\n"), 0o644)
}

func (s *service) entryFilePath(key string) string {
	safeKey := sanitizeFilename(key)
	if safeKey+".md" == indexFilename {
		safeKey = "_" + safeKey
	}
	filename := safeKey + ".md"
	return filepath.Join(s.memoryDir, filename)
}

func (s *service) readEntryLocked(key string) (Entry, error) {
	filePath := s.entryFilePath(key)
	content, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("reading memory file: %w", err)
	}

	fm, body, err := parseMemoryFile(content)
	if err != nil {
		return Entry{}, fmt.Errorf("parsing memory file: %w", err)
	}

	info, statErr := os.Stat(filePath)
	updatedAt := time.Now().UnixNano()
	if statErr == nil {
		updatedAt = info.ModTime().UnixNano()
	}

	return Entry{
		Key:            fm.Key,
		Value:          strings.TrimSpace(body),
		Description:    fm.Description,
		Scope:          fm.Scope,
		Category:       fm.Category,
		Type:           fm.Type,
		Tags:           normalizeTags(fm.Tags),
		Importance:     resolveImportance(fm.Importance, fm.Type),
		AccessCount:    fm.AccessCount,
		LastAccessedAt: fm.LastAccessedAt,
		UpdatedAt:      updatedAt,
	}, nil
}

func (s *service) loadAllEntriesLocked() ([]Entry, error) {
	files, err := os.ReadDir(s.memoryDir)
	if err != nil {
		return nil, fmt.Errorf("reading memory directory: %w", err)
	}

	type fileStat struct {
		name    string
		modTime time.Time
	}
	var mdFiles []fileStat
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") || f.Name() == indexFilename {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		mdFiles = append(mdFiles, fileStat{name: f.Name(), modTime: info.ModTime()})
	}

	sort.Slice(mdFiles, func(i, j int) bool {
		return mdFiles[i].modTime.After(mdFiles[j].modTime)
	})

	if len(mdFiles) > maxMemoryFiles {
		mdFiles = mdFiles[:maxMemoryFiles]
	}

	entries := make([]Entry, 0, len(mdFiles))
	now := time.Now()
	for _, fs := range mdFiles {
		content, err := os.ReadFile(filepath.Join(s.memoryDir, fs.name))
		if err != nil {
			continue
		}
		fm, body, err := parseMemoryFile(content)
		if err != nil {
			continue
		}
		// Drop session-scoped entries past their TTL. They are still on disk
		// (no destructive cleanup here) but invisible to listings/search.
		if isSessionScope(fm.Scope) && now.Sub(fs.modTime) > sessionTTL {
			continue
		}
		entries = append(entries, Entry{
			Key:            fm.Key,
			Value:          strings.TrimSpace(body),
			Description:    fm.Description,
			Scope:          fm.Scope,
			Category:       fm.Category,
			Type:           fm.Type,
			Tags:           normalizeTags(fm.Tags),
			Importance:     resolveImportance(fm.Importance, fm.Type),
			AccessCount:    fm.AccessCount,
			LastAccessedAt: fm.LastAccessedAt,
			UpdatedAt:      fs.modTime.UnixNano(),
		})
	}
	return entries, nil
}

func (s *service) rebuildIndexLocked() error {
	entries, err := s.loadAllEntriesLocked()
	if err != nil {
		return err
	}

	var sb strings.Builder
	sb.WriteString("# Memory Index\n\n")
	sb.WriteString("Auto-generated index of memory entries. Do not edit manually.\n\n")
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.Scope), sessionScope) {
			continue
		}
		desc := truncateForDescription(entry.Value)
		fileName := sanitizeFilename(entry.Key) + ".md"
		fmt.Fprintf(&sb, "- [%s](%s) — %s\n", entry.Key, fileName, desc)
	}
	return os.WriteFile(s.indexPath, []byte(sb.String()), 0o644)
}

func parseMemoryFile(content []byte) (memoryFrontmatter, string, error) {
	text := string(content)
	if !strings.HasPrefix(text, "---\n") {
		return memoryFrontmatter{}, text, nil
	}

	endIdx := strings.Index(text[4:], "\n---\n")
	if endIdx < 0 {
		return memoryFrontmatter{}, text, nil
	}
	endIdx += 4

	fmText := text[4:endIdx]
	body := strings.TrimSpace(text[endIdx+5:])

	var fm memoryFrontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return memoryFrontmatter{}, "", fmt.Errorf("parsing frontmatter: %w", err)
	}
	return fm, body, nil
}

func parseMemoryFrontmatter(content []byte) (memoryFrontmatter, error) {
	text := string(content)
	if !strings.HasPrefix(text, "---\n") {
		return memoryFrontmatter{}, nil
	}

	endIdx := strings.Index(text[4:], "\n---\n")
	if endIdx < 0 {
		return memoryFrontmatter{}, nil
	}
	endIdx += 4

	fmText := text[4:endIdx]
	var fm memoryFrontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return memoryFrontmatter{}, fmt.Errorf("parsing frontmatter: %w", err)
	}
	return fm, nil
}

func readMemoryFileFrontmatter(filePath string) (memoryFrontmatter, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return memoryFrontmatter{}, err
	}
	return parseMemoryFrontmatter(content)
}

func buildMemoryFileContent(fm memoryFrontmatter, body string) string {
	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		fmBytes = []byte(fmt.Sprintf("key: %s\n", fm.Key))
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(fmBytes)
	sb.WriteString("---\n\n")
	sb.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

func sanitizeFilename(key string) string {
	replacer := strings.NewReplacer(
		"/", "__",
		"\\", "__",
		" ", "_",
		":", "-",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	safe := replacer.Replace(key)
	if safe == "" {
		safe = "unnamed"
	}

	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:4])

	if len(safe) > 50 {
		safe = safe[:50]
	}
	return safe + "_" + hashStr
}

func truncateForDescription(value string) string {
	trimmed := strings.TrimSpace(value)
	if len([]rune(trimmed)) <= 120 {
		return trimmed
	}
	return string([]rune(trimmed)[:120]) + "…"
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}

	seen := make(map[string]string, len(tags))
	keys := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		normalized := strings.ToLower(trimmed)
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = trimmed
		keys = append(keys, normalized)
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, seen[key])
	}
	return result
}

type entryFilters struct {
	Scope    string
	Category string
	Type     string
	Tags     []string
}

func shouldIncludeEntry(entry Entry, requestedScope string) bool {
	entryScope := strings.TrimSpace(entry.Scope)
	requestedScope = strings.TrimSpace(requestedScope)
	if strings.EqualFold(requestedScope, sessionScope) {
		return strings.EqualFold(entryScope, sessionScope)
	}
	return !strings.EqualFold(entryScope, sessionScope)
}

func matchesEntryFilters(entry Entry, filters entryFilters) bool {
	if filters.Scope != "" && !strings.EqualFold(strings.TrimSpace(entry.Scope), filters.Scope) {
		return false
	}
	if filters.Category != "" && !strings.EqualFold(strings.TrimSpace(entry.Category), filters.Category) {
		return false
	}
	if filters.Type != "" && !strings.EqualFold(strings.TrimSpace(entry.Type), filters.Type) {
		return false
	}
	if len(filters.Tags) == 0 {
		return true
	}

	entryTags := make(map[string]struct{}, len(entry.Tags))
	for _, tag := range entry.Tags {
		entryTags[strings.ToLower(strings.TrimSpace(tag))] = struct{}{}
	}
	for _, tag := range filters.Tags {
		if _, ok := entryTags[strings.ToLower(tag)]; !ok {
			return false
		}
	}
	return true
}

type memoryEntry Entry

func tokenizeSearchText(text string) []string {
	trimmed := strings.TrimSpace(strings.ToLower(text))
	if trimmed == "" {
		return nil
	}
	splitter := func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r >= 0x4E00 && r <= 0x9FFF:
			return false
		case r >= 0x3040 && r <= 0x30FF:
			return false
		case r >= 0xAC00 && r <= 0xD7AF:
			return false
		default:
			return true
		}
	}
	parts := strings.FieldsFunc(trimmed, splitter)
	if len(parts) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(parts))
	tokens := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		tokens = append(tokens, part)
	}
	return tokens
}

func scoreKeywordMatch(entry Entry, queryTokens []string) float64 {
	if len(queryTokens) == 0 {
		return 0
	}
	fieldTexts := []struct {
		text   string
		weight float64
	}{
		{text: strings.ToLower(entry.Key), weight: 4.0},
		{text: strings.ToLower(entry.Description), weight: 3.5},
		{text: strings.ToLower(entry.Category), weight: 2.0},
		{text: strings.ToLower(entry.Type), weight: 2.0},
		{text: strings.ToLower(strings.Join(entry.Tags, " ")), weight: 2.5},
		{text: strings.ToLower(entry.Value), weight: 1.0},
	}

	score := 0.0
	matchedTokens := 0
	for _, token := range queryTokens {
		tokenMatched := false
		for _, field := range fieldTexts {
			if field.text == "" || !strings.Contains(field.text, token) {
				continue
			}
			score += field.weight
			tokenMatched = true
		}
		if tokenMatched {
			matchedTokens++
		}
	}
	if matchedTokens == 0 {
		return 0
	}
	coverage := float64(matchedTokens) / float64(len(queryTokens))
	return score + coverage*5.0
}

// entryScore combines importance, recency, and access frequency into a single
// ranking signal. Higher score = more relevant.
//
//	score = importance*0.5 + recency*0.3 + frequency*0.2
//
// recency decays linearly to zero over 30 days; frequency saturates at ~10 hits.
func entryScore(entry Entry, now time.Time) float64 {
	importance := resolveImportance(entry.Importance, entry.Type)

	const recencyHalflife = 30 * 24 * time.Hour
	age := now.Sub(time.Unix(0, entry.UpdatedAt))
	recency := 1.0 - float64(age)/float64(recencyHalflife)
	if recency < 0 {
		recency = 0
	}
	if recency > 1 {
		recency = 1
	}

	// frequency: 1 - 1/(1+count/10), so 0->0, 10->0.5, +inf->1
	frequency := 0.0
	if entry.AccessCount > 0 {
		frequency = float64(entry.AccessCount) / (float64(entry.AccessCount) + 10.0)
	}

	return importance*0.5 + recency*0.3 + frequency*0.2
}

func sortEntries(entries []Entry) {
	now := time.Now()
	sort.Slice(entries, func(i, j int) bool {
		si, sj := entryScore(entries[i], now), entryScore(entries[j], now)
		if si != sj {
			return si > sj
		}
		if entries[i].UpdatedAt != entries[j].UpdatedAt {
			return entries[i].UpdatedAt > entries[j].UpdatedAt
		}
		return entries[i].Key < entries[j].Key
	})
}

// clampImportance bounds an importance value to [0, 1].
func clampImportance(v float64) float64 {
	if v < minImportance {
		return minImportance
	}
	if v > maxImportance {
		return maxImportance
	}
	return v
}

// defaultImportanceForType returns a sensible default importance based on the
// memory type. user/feedback memories are weighted higher because they encode
// persistent constraints; session memories are lower because they are transient.
func defaultImportanceForType(memType string) float64 {
	switch strings.ToLower(strings.TrimSpace(memType)) {
	case "user", "feedback":
		return 0.8
	case "project", "reference":
		return 0.6
	case "session":
		return 0.3
	default:
		return defaultImportance
	}
}

// resolveImportance returns the entry's stored importance if set, otherwise the
// type-based default. Used during reads so older entries without the field still
// rank sensibly.
func resolveImportance(stored float64, memType string) float64 {
	if stored > 0 {
		return clampImportance(stored)
	}
	return defaultImportanceForType(memType)
}

// isSessionScope reports whether the given scope value represents the
// session scope (case-insensitive).
func isSessionScope(scope string) bool {
	return strings.EqualFold(strings.TrimSpace(scope), sessionScope)
}

// writeAccessMetaLocked persists access counters back to the memory file.
// It is called only when AccessCount reaches a batch threshold to avoid a
// random write on every Get call.
func (s *service) writeAccessMetaLocked(entry Entry) error {
	fm := memoryFrontmatter{
		Key:            entry.Key,
		Description:    entry.Description,
		Scope:          entry.Scope,
		Category:       entry.Category,
		Type:           entry.Type,
		Tags:           entry.Tags,
		Importance:     entry.Importance,
		AccessCount:    entry.AccessCount,
		LastAccessedAt: entry.LastAccessedAt,
	}
	if fm.Description == "" {
		fm.Description = truncateForDescription(entry.Value)
	}
	content := buildMemoryFileContent(fm, entry.Value)
	return os.WriteFile(s.entryFilePath(entry.Key), []byte(content), 0o644)
}

func applyLimit(entries []Entry, limit int) []Entry {
	normalized := limit
	if normalized <= 0 {
		normalized = defaultLimit
	}
	if normalized > maxLimit {
		normalized = maxLimit
	}
	if normalized >= len(entries) {
		return entries
	}
	return entries[:normalized]
}

// ReadIndex returns the raw content of the MEMORY.md index file.
func (s *service) ReadIndex() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	content, err := os.ReadFile(s.indexPath)
	if err != nil {
		return "", fmt.Errorf("reading memory index: %w", err)
	}
	return string(content), nil
}

// MemoryFileInfo holds metadata about a memory file without loading full content.
type MemoryFileInfo struct {
	Key         string
	FileName    string
	Description string
	Scope       string
	Category    string
	Type        string
	Tags        []string
	UpdatedAt   int64
}

// ListMemoryFiles returns metadata about all memory files without loading full content.
func (s *service) ListMemoryFiles() ([]MemoryFileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	files, err := os.ReadDir(s.memoryDir)
	if err != nil {
		return nil, fmt.Errorf("reading memory directory: %w", err)
	}

	type fileInfo struct {
		name      string
		updatedAt int64
	}
	candidates := make([]fileInfo, 0, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") || f.Name() == indexFilename {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, fileInfo{name: f.Name(), updatedAt: info.ModTime().UnixNano()})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].updatedAt > candidates[j].updatedAt
	})

	if len(candidates) > maxMemoryFiles {
		candidates = candidates[:maxMemoryFiles]
	}

	infos := make([]MemoryFileInfo, 0, len(candidates))
	for _, candidate := range candidates {
		fm, err := readMemoryFileFrontmatter(filepath.Join(s.memoryDir, candidate.name))
		if err != nil {
			continue
		}
		infos = append(infos, MemoryFileInfo{
			Key:         fm.Key,
			FileName:    candidate.name,
			Description: fm.Description,
			Scope:       fm.Scope,
			Category:    fm.Category,
			Type:        fm.Type,
			Tags:        normalizeTags(fm.Tags),
			UpdatedAt:   candidate.updatedAt,
		})
	}
	return infos, nil
}

// ReadMemoryFileBody reads just the body content of a memory file by filename.
func (s *service) ReadMemoryFileBody(fileName string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fullPath := filepath.Join(s.memoryDir, fileName)
	cleanPath := filepath.Clean(fullPath)
	cleanDir := filepath.Clean(s.memoryDir) + string(filepath.Separator)
	if !strings.HasPrefix(cleanPath, cleanDir) {
		return "", fmt.Errorf("invalid memory file path")
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("reading memory file: %w", err)
	}
	_, body, err := parseMemoryFile(content)
	return strings.TrimSpace(body), err
}

type consolidationLockState struct {
	Owner      string `json:"owner"`
	AcquiredAt int64  `json:"acquired_at"`
}

func (s *service) consolidatedAtPath() string {
	return filepath.Join(s.memoryDir, consolidatedAtFilename)
}

func (s *service) consolidationLockPath() string {
	return filepath.Join(s.memoryDir, consolidationLockFilename)
}

func (s *service) ReadLastConsolidatedAt() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.readLastConsolidatedAtLocked()
}

func (s *service) readLastConsolidatedAtLocked() (time.Time, error) {
	content, err := os.ReadFile(s.consolidatedAtPath())
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("reading last consolidated timestamp: %w", err)
	}

	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return time.Time{}, nil
	}

	unixSeconds, err := strconv.ParseInt(trimmed, 10, 64)
	if err == nil {
		return time.Unix(unixSeconds, 0), nil
	}

	parsed, parseErr := time.Parse(time.RFC3339Nano, trimmed)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("parsing last consolidated timestamp: %w", parseErr)
	}
	return parsed, nil
}

func (s *service) WriteLastConsolidatedAt(at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	content := strconv.FormatInt(at.Unix(), 10)
	if err := os.WriteFile(s.consolidatedAtPath(), []byte(content+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing last consolidated timestamp: %w", err)
	}
	return nil
}

func (s *service) TryAcquireConsolidationLock(owner string, staleAfter time.Duration) (bool, error) {
	normalizedOwner := strings.TrimSpace(owner)
	if normalizedOwner == "" {
		return false, fmt.Errorf("owner is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	lockPath := s.consolidationLockPath()
	now := time.Now()
	payload, err := json.Marshal(consolidationLockState{Owner: normalizedOwner, AcquiredAt: now.Unix()})
	if err != nil {
		return false, fmt.Errorf("marshaling consolidation lock: %w", err)
	}

	writeLock := func() (bool, error) {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			defer file.Close()
			if _, err := file.Write(payload); err != nil {
				return false, fmt.Errorf("writing consolidation lock: %w", err)
			}
			return true, nil
		}
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating consolidation lock: %w", err)
	}

	acquired, err := writeLock()
	if err != nil || acquired {
		return acquired, err
	}

	existingBytes, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return writeLock()
		}
		return false, fmt.Errorf("reading consolidation lock: %w", err)
	}

	var existing consolidationLockState
	if err := json.Unmarshal(existingBytes, &existing); err != nil {
		if removeErr := os.Remove(lockPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return false, fmt.Errorf("removing corrupt consolidation lock: %w", removeErr)
		}
		return writeLock()
	}

	if existing.Owner == normalizedOwner {
		return true, nil
	}

	if staleAfter > 0 && existing.AcquiredAt > 0 && now.Sub(time.Unix(existing.AcquiredAt, 0)) >= staleAfter {
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("removing stale consolidation lock: %w", err)
		}
		return writeLock()
	}

	return false, nil
}

func (s *service) ReleaseConsolidationLock(owner string) error {
	normalizedOwner := strings.TrimSpace(owner)
	if normalizedOwner == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	lockPath := s.consolidationLockPath()
	content, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading consolidation lock: %w", err)
	}

	var existing consolidationLockState
	if err := json.Unmarshal(content, &existing); err != nil {
		if removeErr := os.Remove(lockPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("removing corrupt consolidation lock: %w", removeErr)
		}
		return nil
	}

	if existing.Owner != "" && existing.Owner != normalizedOwner {
		return nil
	}

	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing consolidation lock: %w", err)
	}
	return nil
}
