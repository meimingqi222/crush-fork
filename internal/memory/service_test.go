package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryServiceStoreGetDelete(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	err = service.Store(context.Background(), StoreParams{Key: "project/goal", Value: "Ship MVP", Scope: "project", Category: "product", Type: "goal", Tags: []string{"roadmap", "launch"}})
	require.NoError(t, err)

	entry, err := service.Get(context.Background(), "project/goal")
	require.NoError(t, err)
	require.Equal(t, "project/goal", entry.Key)
	require.Equal(t, "Ship MVP", entry.Value)
	require.Equal(t, "project", entry.Scope)
	require.Equal(t, "product", entry.Category)
	require.Equal(t, "goal", entry.Type)
	require.Equal(t, []string{"launch", "roadmap"}, entry.Tags)
	require.NotZero(t, entry.UpdatedAt)

	err = service.Delete(context.Background(), "project/goal")
	require.NoError(t, err)

	_, err = service.Get(context.Background(), "project/goal")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryServiceSearchAndList(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "alpha", Value: "first memory", Scope: "project", Category: "notes", Type: "fact", Tags: []string{"alpha"}}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "beta", Value: "second memory", Scope: "project", Category: "preferences", Type: "workflow", Tags: []string{"golang", "tests"}}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "auth/jwt-validation", Value: "Validates JWT token claims during login", Description: "Authentication flow", Scope: "project", Category: "security", Type: "decision", Tags: []string{"auth", "jwt"}, Importance: 0.9}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "session-note", Value: "beta plan", Scope: "session", Category: "notes", Type: "plan", Tags: []string{"beta", "tests"}}))

	searchResults, err := service.Search(context.Background(), SearchParams{Query: "beta", Limit: 10})
	require.NoError(t, err)
	require.Len(t, searchResults, 1)
	require.Equal(t, "beta", searchResults[0].Key)

	sessionOnly, err := service.Search(context.Background(), SearchParams{Query: "beta", Scope: "session", Limit: 10})
	require.NoError(t, err)
	require.Len(t, sessionOnly, 1)
	require.Equal(t, "session-note", sessionOnly[0].Key)

	metadataSearch, err := service.Search(context.Background(), SearchParams{Query: "workflow", Type: "workflow", Tags: []string{"golang"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, metadataSearch, 1)
	require.Equal(t, "beta", metadataSearch[0].Key)

	projectOnly, err := service.Search(context.Background(), SearchParams{Query: "beta", Scope: "project", Limit: 10})
	require.NoError(t, err)
	require.Len(t, projectOnly, 1)
	require.Equal(t, "beta", projectOnly[0].Key)

	multiToken, err := service.Search(context.Background(), SearchParams{Query: "jwt auth", Scope: "project", Limit: 10})
	require.NoError(t, err)
	require.Len(t, multiToken, 1)
	require.Equal(t, "auth/jwt-validation", multiToken[0].Key)

	punctuated, err := service.Search(context.Background(), SearchParams{Query: "auth jwt-validation", Scope: "project", Limit: 10})
	require.NoError(t, err)
	require.Len(t, punctuated, 1)
	require.Equal(t, "auth/jwt-validation", punctuated[0].Key)

	coverageRanked, err := service.Search(context.Background(), SearchParams{Query: "jwt validation login", Scope: "project", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, coverageRanked)
	require.Equal(t, "auth/jwt-validation", coverageRanked[0].Key)

	listResults, err := service.List(context.Background(), ListParams{Scope: "project", Limit: 3})
	require.NoError(t, err)
	require.Len(t, listResults, 3)
	require.Equal(t, "auth/jwt-validation", listResults[0].Key)

	tagFiltered, err := service.List(context.Background(), ListParams{Category: "notes", Tags: []string{"beta"}, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, tagFiltered)

	sessionList, err := service.List(context.Background(), ListParams{Scope: "session", Limit: 10})
	require.NoError(t, err)
	require.Len(t, sessionList, 1)
	require.Equal(t, "session-note", sessionList[0].Key)
}

func TestMemoryServiceStoresAsMarkdownFiles(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewService(dataDir)
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "test-entry", Value: "hello world", Scope: "project", Type: "fact"}))

	memoryDir := filepath.Join(dataDir, "memory")
	files, err := os.ReadDir(memoryDir)
	require.NoError(t, err)

	var mdFiles []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") && f.Name() != indexFilename {
			mdFiles = append(mdFiles, f.Name())
		}
	}
	require.Len(t, mdFiles, 1)
	require.True(t, strings.HasSuffix(mdFiles[0], ".md"))

	content, err := os.ReadFile(filepath.Join(memoryDir, mdFiles[0]))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(content), "---\n"))
	require.Contains(t, string(content), "key: test-entry")
	require.Contains(t, string(content), "---\n\nhello world")
}

func TestMemoryServiceRebuildsIndex(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewService(dataDir)
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "entry-one", Value: "first value"}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "entry-two", Value: "second value"}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "session/current", Value: "volatile", Scope: "session"}))

	indexContent, err := service.ReadIndex()
	require.NoError(t, err)
	require.Contains(t, indexContent, "entry-one")
	require.Contains(t, indexContent, "entry-two")
	require.NotContains(t, indexContent, "session/current")

	require.NoError(t, service.Delete(context.Background(), "entry-one"))

	indexContent, err = service.ReadIndex()
	require.NoError(t, err)
	require.NotContains(t, indexContent, "entry-one")
	require.Contains(t, indexContent, "entry-two")
}

func TestMemoryServiceListMemoryFiles(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "k1", Value: "value one", Type: "user"}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "k2", Value: "value two", Type: "feedback"}))

	infos, err := service.ListMemoryFiles()
	require.NoError(t, err)
	require.Len(t, infos, 2)
	require.Equal(t, "k2", infos[0].Key)
	require.Equal(t, "k1", infos[1].Key)
}

func TestMemoryServiceListMemoryFilesSkipsInvalidFrontmatter(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewService(dataDir)
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "valid", Value: "valid body", Type: "project"}))

	memoryDir := filepath.Join(dataDir, "memory")
	require.NoError(t, os.WriteFile(filepath.Join(memoryDir, "broken.md"), []byte("---\nkey: [\n---\n\nbroken\n"), 0o644))

	infos, err := service.ListMemoryFiles()
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, "valid", infos[0].Key)
}

func TestMemoryServiceReadMemoryFileBody(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "body-test", Value: "the actual body content"}))

	infos, err := service.ListMemoryFiles()
	require.NoError(t, err)
	require.Len(t, infos, 1)

	body, err := service.ReadMemoryFileBody(infos[0].FileName)
	require.NoError(t, err)
	require.Equal(t, "the actual body content", body)

	_, err = service.ReadMemoryFileBody("../../../etc/passwd")
	require.ErrorContains(t, err, "invalid memory file path")
}

func TestParseMemoryFrontmatter(t *testing.T) {
	t.Parallel()

	fm, err := parseMemoryFrontmatter([]byte("---\nkey: test\ndescription: desc\ntype: project\n---\n\nbody\n"))
	require.NoError(t, err)
	require.Equal(t, "test", fm.Key)
	require.Equal(t, "desc", fm.Description)
	require.Equal(t, "project", fm.Type)

	empty, err := parseMemoryFrontmatter([]byte("body only"))
	require.NoError(t, err)
	require.Equal(t, memoryFrontmatter{}, empty)

	_, err = parseMemoryFrontmatter([]byte("---\nkey: [\n---\n\nbody\n"))
	require.Error(t, err)
}

func TestMemoryServiceValidation(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	err = service.Store(context.Background(), StoreParams{Key: " ", Value: "value"})
	require.ErrorContains(t, err, "key is required")

	err = service.Store(context.Background(), StoreParams{Key: "key", Value: " "})
	require.ErrorContains(t, err, "value is required")

	_, err = service.Search(context.Background(), SearchParams{Query: " ", Limit: 10})
	require.ErrorContains(t, err, "query is required")

	err = service.Delete(context.Background(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryServiceContextCancellation(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = service.Store(ctx, StoreParams{Key: "key", Value: "value"})
	require.True(t, errors.Is(err, context.Canceled))
}

func TestMemoryServiceSanitizeFilename(t *testing.T) {
	t.Parallel()

	require.Contains(t, sanitizeFilename("hello world"), "hello_world")
	require.Contains(t, sanitizeFilename("path/to/file"), "path__to__file")
	require.Contains(t, sanitizeFilename("   "), "___")
	require.Contains(t, sanitizeFilename("colon:separated"), "colon-separated")

	require.NotEqual(t, sanitizeFilename("hello world"), sanitizeFilename("hello_world"))
	require.Contains(t, sanitizeFilename("hello world"), "_")
	require.Contains(t, sanitizeFilename("hello_world"), "_")
}

func TestMemoryServiceTruncateForDescription(t *testing.T) {
	t.Parallel()

	short := "short value"
	require.Equal(t, short, truncateForDescription(short))

	long := strings.Repeat("a", 200)
	result := truncateForDescription(long)
	require.Len(t, []rune(result), 121)
	require.True(t, strings.HasSuffix(result, "…"))
}

func TestMemoryServiceCustomDescriptionAndImportance(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:         "k",
		Value:       strings.Repeat("body ", 40),
		Description: "explicit summary",
		Importance:  0.9,
		Type:        "project",
	}))

	entry, err := service.Get(context.Background(), "k")
	require.NoError(t, err)
	require.Equal(t, "explicit summary", entry.Description)
	require.InDelta(t, 0.9, entry.Importance, 0.0001)
	require.GreaterOrEqual(t, entry.AccessCount, int64(1))
}

func TestMemoryServiceImportanceAffectsRanking(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	// "low" stored first (older mtime), "high" stored second.
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "low", Value: "low importance", Type: "project", Importance: 0.1}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "high", Value: "high importance", Type: "project", Importance: 1.0}))

	entries, err := service.List(context.Background(), ListParams{Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "high", entries[0].Key, "high-importance entry should rank first regardless of recency tie")
}

func TestMemoryServiceSessionTTL(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewService(dataDir)
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "stale-session", Value: "old", Scope: "session"}))
	require.NoError(t, service.Store(context.Background(), StoreParams{Key: "fresh-session", Value: "new", Scope: "session"}))

	// Backdate the stale session file past the TTL.
	memoryDir := filepath.Join(dataDir, "memory")
	files, err := os.ReadDir(memoryDir)
	require.NoError(t, err)
	for _, f := range files {
		if strings.Contains(f.Name(), "stale-session") {
			old := time.Now().Add(-(sessionTTL + time.Hour))
			require.NoError(t, os.Chtimes(filepath.Join(memoryDir, f.Name()), old, old))
		}
	}

	entries, err := service.List(context.Background(), ListParams{Scope: "session", Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "fresh-session", entries[0].Key)
}

func TestMemoryServiceStorePreservesUnflushedAccessCount(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	// Store an entry so it exists on disk with AccessCount=0.
	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "counter-key",
		Value: "initial value",
		Type:  "project",
	}))

	// Call Get fewer times than accessFlushBatch so the increments are
	// not yet flushed to disk.
	const getsBeforeStore = 3
	for i := 0; i < getsBeforeStore; i++ {
		entry, err := service.Get(context.Background(), "counter-key")
		require.NoError(t, err)
		require.Equal(t, int64(i+1), entry.AccessCount, "in-memory counter should increment")
	}

	// Store should merge the pending increments instead of overwriting
	// with the stale disk value.
	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "counter-key",
		Value: "updated value",
		Type:  "project",
	}))

	entry, err := service.Get(context.Background(), "counter-key")
	require.NoError(t, err)
	require.GreaterOrEqual(t, entry.AccessCount, int64(getsBeforeStore+1),
		"Store must preserve unflushed access-count increments from Get")
}

func TestMemoryServiceStoreLastAccessedAtMonotonic(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	// Store initial entry.
	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "time-key",
		Value: "initial",
		Type:  "project",
	}))

	// Get to bump LastAccessedAt.
	entry1, err := service.Get(context.Background(), "time-key")
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)

	// Get again to increment count but not flush (assuming batch > 1).
	entry2, err := service.Get(context.Background(), "time-key")
	require.NoError(t, err)
	require.Greater(t, entry2.LastAccessedAt, entry1.LastAccessedAt)

	// Store should preserve the newer LastAccessedAt even if counts are equal.
	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "time-key",
		Value: "updated",
		Type:  "project",
	}))

	entry3, err := service.Get(context.Background(), "time-key")
	require.NoError(t, err)
	require.GreaterOrEqual(t, entry3.LastAccessedAt, entry2.LastAccessedAt,
		"LastAccessedAt must not go backwards after Store")
}

func TestMemoryServiceNewEntryLastAccessedAt(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	// Store a brand new entry (no prior Get, no pending state).
	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "new-key",
		Value: "brand new",
		Type:  "project",
	}))

	entry, err := service.Get(context.Background(), "new-key")
	require.NoError(t, err)
	require.NotZero(t, entry.LastAccessedAt,
		"New entries must have a non-zero LastAccessedAt")
	require.Greater(t, entry.LastAccessedAt, int64(0),
		"LastAccessedAt should be a valid timestamp, not Unix epoch")
}

func TestMemoryServiceStoreDetectsDuplicates(t *testing.T) {
	t.Parallel()

	service, err := NewService(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "project/auth-flow",
		Value: "Use JWT tokens for authentication with a 24-hour expiration",
		Scope: "project",
		Type:  "decision",
	}))

	// Storing with a different key but very similar value should be rejected.
	err = service.Store(context.Background(), StoreParams{
		Key:   "project/auth-decision",
		Value: "Use JWT tokens for authentication with a 24 hour expiration",
		Scope: "project",
		Type:  "decision",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "similar memory already exists")
	require.Contains(t, err.Error(), "project/auth-flow")

	// Storing a clearly different value should succeed.
	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "project/db-choice",
		Value: "Use PostgreSQL for the primary database",
		Scope: "project",
		Type:  "decision",
	}))

	// Updating the existing key (same key) should succeed regardless of similarity.
	require.NoError(t, service.Store(context.Background(), StoreParams{
		Key:   "project/auth-flow",
		Value: "Use JWT tokens for authentication with a 24-hour expiration and refresh tokens",
		Scope: "project",
		Type:  "decision",
	}))
}
