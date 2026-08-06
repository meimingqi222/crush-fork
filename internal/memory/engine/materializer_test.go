package engine

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArtifactWriter_WriteAndRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := NewArtifactWriter(dir)

	err := w.WriteFile("test.txt", []byte("hello"))
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "test.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))

	err = w.RemoveFile("test.txt")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "test.txt"))
	require.True(t, os.IsNotExist(err))
}

func TestArtifactWriter_CreatesDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := NewArtifactWriter(dir)

	err := w.WriteFile("nested/a/b/c.txt", []byte("deep"))
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "nested", "a", "b", "c.txt"))
	require.NoError(t, err)
}

func TestSummaryMaterializer_EmptyStore(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()

	m := NewSummaryMaterializer(db, store, NewArtifactWriter(dir))
	ctx := context.Background()

	err := m.Materialize(ctx, "memory_summary", nil)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "memory_summary.md"))
	require.True(t, os.IsNotExist(err), "should not write file with no events")
}

func TestSummaryMaterializer_WritesSummary(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	events := []MemoryEvent{
		testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite for storage"),
		testEvent(MemoryScopeUser, MemoryKindPreference, "User prefers dark theme"),
		testEvent(MemoryScopeProject, MemoryKindPitfall, "Avoid global state in handlers"),
	}
	for _, evt := range events {
		require.NoError(t, store.Append(ctx, evt))
	}

	m := NewSummaryMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "memory_summary", nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "memory_summary.md"))
	require.NoError(t, err)

	content := string(data)
	require.Contains(t, content, "# Memory Summary")
	require.Contains(t, content, "Decisions")
	require.Contains(t, content, "Preferences")
	require.Contains(t, content, "Pitfalls & Gotchas")
	require.Contains(t, content, "Use SQLite for storage")
	require.Contains(t, content, "watermark")
}

func TestSummaryMaterializer_WatermarkAdvances(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	evt1 := testEvent(MemoryScopeProject, MemoryKindDecision, "First decision")
	require.NoError(t, store.Append(ctx, evt1))

	m := NewSummaryMaterializer(db, store, NewArtifactWriter(dir))

	// First pass — materialize and advance watermark.
	err := m.Materialize(ctx, "memory_summary", nil)
	require.NoError(t, err)

	wm, err := readViewWatermark(ctx, db, "memory_summary")
	require.NoError(t, err)
	require.Greater(t, wm, int64(0), "watermark should advance after first materialize")

	// Second pass with no new events — watermark unchanged.
	err = m.Materialize(ctx, "memory_summary", nil)
	require.NoError(t, err)

	wm2, err := readViewWatermark(ctx, db, "memory_summary")
	require.NoError(t, err)
	require.Equal(t, wm, wm2, "watermark should not advance with no new events")
}

func TestMemoryMDMaterializer_WritesFullDoc(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	events := []MemoryEvent{
		testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite for storage"),
		testEvent(MemoryScopeUser, MemoryKindPreference, "User prefers dark theme"),
		testEvent(MemoryScopeProject, MemoryKindProcedure, "Deploy steps: build, test, push"),
	}
	for _, evt := range events {
		require.NoError(t, store.Append(ctx, evt))
	}

	m := NewMemoryMDMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "MEMORY", nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	require.NoError(t, err)

	content := string(data)
	require.Contains(t, content, "# MEMORY.md")
	require.Contains(t, content, "Long-Term Memory")
	require.Contains(t, content, "Project")
	require.Contains(t, content, "User")
	require.Contains(t, content, "Use SQLite for storage")
	require.Contains(t, content, "User prefers dark theme")
	require.Contains(t, content, "Deploy steps")
}

func TestMemoryMDMaterializer_EmptyStore(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()

	m := NewMemoryMDMaterializer(db, store, NewArtifactWriter(dir))
	ctx := context.Background()

	err := m.Materialize(ctx, "MEMORY", nil)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "MEMORY.md"))
	require.True(t, os.IsNotExist(err), "should not write file with no events")
}

func TestMemoryMDMaterializer_WatermarkAdvances(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	evt1 := testEvent(MemoryScopeProject, MemoryKindDecision, "Decision 1")
	require.NoError(t, store.Append(ctx, evt1))

	m := NewMemoryMDMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "MEMORY", nil)
	require.NoError(t, err)

	wm, err := readViewWatermark(ctx, db, "MEMORY")
	require.NoError(t, err)
	require.Greater(t, wm, int64(0))

	// No new events — watermark unchanged.
	err = m.Materialize(ctx, "MEMORY", nil)
	require.NoError(t, err)

	wm2, err := readViewWatermark(ctx, db, "MEMORY")
	require.NoError(t, err)
	require.Equal(t, wm, wm2)
}

func TestSkillsMaterializer_BelowThreshold(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	// 1 procedure event, threshold is 3.
	evt := testEvent(MemoryScopeProject, MemoryKindProcedure, "How to deploy")
	require.NoError(t, store.Append(ctx, evt))

	m := NewSkillsMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "skills", nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "skills/SKILL.md"))
	require.NoError(t, err)

	content := string(data)
	require.Contains(t, content, "placeholder")
	require.Contains(t, content, "How to deploy")
}

func TestSkillsMaterializer_AboveThreshold(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	// 3 procedure events with default high confidence.
	for i := 0; i < 3; i++ {
		evt := testEvent(MemoryScopeProject, MemoryKindProcedure,
			"Procedure step for scenario")
		require.NoError(t, store.Append(ctx, evt))
	}

	m := NewSkillsMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "skills", nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "skills/SKILL.md"))
	require.NoError(t, err)

	content := string(data)
	require.Contains(t, content, "# Skills")
	require.Contains(t, content, "stable procedural memories")
	require.NotContains(t, content, "placeholder")
}

func TestSkillsMaterializer_LowConfidenceFilter(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	// 3 procedure events but low confidence.
	for i := 0; i < 3; i++ {
		evt := testEvent(MemoryScopeProject, MemoryKindProcedure,
			"Low confidence procedure")
		evt.Confidence = 0.3
		require.NoError(t, store.Append(ctx, evt))
	}

	m := NewSkillsMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "skills", nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "skills/SKILL.md"))
	require.NoError(t, err)

	content := string(data)
	require.Contains(t, content, "below confidence threshold")
}

func TestSkillsMaterializer_EmptyStore(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()

	m := NewSkillsMaterializer(db, store, NewArtifactWriter(dir))
	ctx := context.Background()

	err := m.Materialize(ctx, "skills", nil)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "skills/SKILL.md"))
	require.True(t, os.IsNotExist(err), "should not write file with no procedure events")
}

func TestEngine_TriggerMaterialization(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	eng := New(db, Config{Enabled: true})
	eng.store = store

	evt := testEvent(MemoryScopeProject, MemoryKindDecision, "Test decision")
	require.NoError(t, store.Append(ctx, evt))

	summaryMat := NewSummaryMaterializer(db, store, NewArtifactWriter(dir))
	eng.SetMaterializer(summaryMat)

	err := eng.TriggerMaterialization(ctx)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "memory_summary.md"))
	require.NoError(t, err)
}

func TestEngine_TriggerMaterializationDisabled(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	eng := New(db, Config{Enabled: false})
	eng.store = store

	evt := testEvent(MemoryScopeProject, MemoryKindDecision, "Test decision")
	require.NoError(t, store.Append(ctx, evt))

	m := NewSummaryMaterializer(db, store, NewArtifactWriter(dir))
	eng.SetMaterializer(m)

	err := eng.TriggerMaterialization(ctx)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "memory_summary.md"))
	require.True(t, os.IsNotExist(err), "should not write when engine is disabled")
}

func TestEngine_MultiMaterializer(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	eng := New(db, Config{Enabled: true})
	eng.store = store

	evts := []MemoryEvent{
		testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite"),
		testEvent(MemoryScopeUser, MemoryKindPreference, "Dark theme"),
		testEvent(MemoryScopeProject, MemoryKindProcedure, "Deploy steps"),
	}
	for _, evt := range evts {
		require.NoError(t, store.Append(ctx, evt))
	}

	writer := NewArtifactWriter(dir)
	eng.SetMaterializer(NewSummaryMaterializer(db, store, writer))
	eng.SetMaterializer(NewMemoryMDMaterializer(db, store, writer))
	eng.SetMaterializer(NewSkillsMaterializer(db, store, writer))

	err := eng.TriggerMaterialization(ctx)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "memory_summary.md"))
	require.NoError(t, err, "memory_summary.md should exist")

	_, err = os.Stat(filepath.Join(dir, "MEMORY.md"))
	require.NoError(t, err, "MEMORY.md should exist")

	_, err = os.Stat(filepath.Join(dir, "skills", "SKILL.md"))
	require.NoError(t, err, "skills/SKILL.md should exist")
}

func TestSummaryMaterializer_SortsByImportance(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	evt1 := testEvent(MemoryScopeProject, MemoryKindDecision, "Important decision")
	evt1.Importance = 0.9
	evt2 := testEvent(MemoryScopeProject, MemoryKindDecision, "Less important decision")
	evt2.Importance = 0.3
	evt3 := testEvent(MemoryScopeProject, MemoryKindDecision, "Medium decision")
	evt3.Importance = 0.6

	for _, evt := range []MemoryEvent{evt1, evt2, evt3} {
		require.NoError(t, store.Append(ctx, evt))
	}

	m := NewSummaryMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "memory_summary", nil)
	require.NoError(t, err)

	// Verify the file was written successfully.
	data, err := os.ReadFile(filepath.Join(dir, "memory_summary.md"))
	require.NoError(t, err)
	require.Contains(t, string(data), "Important decision")
	require.Contains(t, string(data), "Medium decision")
	require.Contains(t, string(data), "Less important decision")
}

func TestMemoryMDMaterializer_IncludesWatermark(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	dir := t.TempDir()
	ctx := context.Background()

	evt := testEvent(MemoryScopeProject, MemoryKindReference, "Reference doc")
	require.NoError(t, store.Append(ctx, evt))

	m := NewMemoryMDMaterializer(db, store, NewArtifactWriter(dir))
	err := m.Materialize(ctx, "MEMORY", nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	require.NoError(t, err)
	require.Contains(t, string(data), "Watermark")
}

// readViewWatermark reads the watermark for a materialized view from SQLite.
func readViewWatermark(ctx context.Context, db *sql.DB, viewName string) (int64, error) {
	var wm int64
	err := db.QueryRowContext(ctx,
		"SELECT COALESCE(watermark, 0) FROM memory_materialized_views WHERE view_name = ?",
		viewName).Scan(&wm)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return wm, nil
}
