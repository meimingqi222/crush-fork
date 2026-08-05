package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed todo.md
var todoDescription string

// TodoToolName is the name of the todo tool.
const TodoToolName = "todo"

const maxTodoItems = 50

// TodoParams defines the parameters for the todo tool.
type TodoParams struct {
	Op       string   `json:"op" description:"Operation: init, add, start, done, block, unblock, drop, view"`
	Index    int      `json:"index,omitempty" description:"Zero-based task index (required for start, done, block, unblock, drop)"`
	Items    []string `json:"items,omitempty" description:"Task content list (required for init and add)"`
	Reason   string   `json:"reason,omitempty" description:"Reason for block or drop"`
	Content  string   `json:"content,omitempty" description:"Task content (optional, used to identify task for done/block/drop)"`
	Evidence string   `json:"evidence,omitempty" description:"Evidence that the task is truly complete (optional for done, but required by goal completion gate)"`
}

// TodoResponseMetadata is attached to tool responses for UI rendering.
type TodoResponseMetadata struct {
	Tasks []session.Task `json:"tasks"`
}

// NewTodoTool creates a new todo tool instance. maxItems caps the number of
// tasks allowed in a goal's task list; a value of 0 or less falls back to
// the package default (50).
func NewTodoTool(sessions session.Service, goalRuntime *goal.Runtime, maxItems int) fantasy.AgentTool {
	if maxItems <= 0 {
		maxItems = maxTodoItems
	}
	return fantasy.NewAgentTool(
		TodoToolName,
		string(todoDescription),
		func(ctx context.Context, params TodoParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if isSubagent := IsSubagentFromContext(ctx); isSubagent {
				return handleSubagentTodo(ctx, sessions, goalRuntime, params, call)
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("todo tool requires a session context")
			}

			sess, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}
			if !sess.Goal.IsActive() {
				return fantasy.ToolResponse{}, fmt.Errorf("todo tool requires an active goal")
			}

			oldTasks := sess.Goal.Tasks
			var tasks []session.Task
			switch params.Op {
			case "init":
				tasks, err = todoInit(sess.Goal, params.Items, maxItems)
			case "add":
				tasks, err = todoAdd(sess.Goal, params.Items, maxItems)
			case "start":
				tasks, err = todoStart(sess.Goal, params.Index, params.Content)
			case "done":
				tasks, err = todoDone(sess.Goal, params.Index, params.Content, params.Evidence)
			case "block":
				tasks, err = todoBlock(sess.Goal, params.Index, params.Content, params.Reason)
			case "unblock":
				tasks, err = todoUnblock(sess.Goal, params.Index, params.Content)
			case "drop":
				tasks, err = todoDrop(sess.Goal, params.Index, params.Content, params.Reason)
			case "view":
				tasks = sess.Goal.Tasks
			default:
				return fantasy.ToolResponse{}, fmt.Errorf("unknown operation: %s (expected: init, add, start, done, block, unblock, drop, view)", params.Op)
			}
			if err != nil {
				return fantasy.ToolResponse{}, err
			}

			sess.Goal.Tasks = tasks
			sess.Goal.UpdatedAt = time.Now().Unix()

			// Drive the stall counter: real progress (pending/in_progress -> completed) resets;
			// dropping a task counts as no progress.
			switch params.Op {
			case "done":
				if i, resolveErr := resolveTaskIndex(oldTasks, params.Index, params.Content); resolveErr == nil && i >= 0 {
					if oldTasks[i].Status != session.TaskStatusCompleted {
						sess.Goal.NoProgress = 0
					}
				}
			case "drop":
				if i, resolveErr := resolveTaskIndex(oldTasks, params.Index, params.Content); resolveErr == nil && i >= 0 {
					if oldTasks[i].Status != session.TaskStatusDropped {
						sess.Goal.NoProgress++
					}
				}
			}

			if _, saveErr := sessions.Save(ctx, sess); saveErr != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save session: %w", saveErr)
			}

			text := formatTodoResponse(tasks, params.Op)
			metadata, err := json.Marshal(TodoResponseMetadata{Tasks: tasks})
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to encode todo metadata: %w", err)
			}

			return fantasy.ToolResponse{
				Content:  text,
				Metadata: string(metadata),
			}, nil
		},
	)
}

func todoInit(goal session.Goal, items []string, maxItems int) ([]session.Task, error) {
	if len(goal.Tasks) > 0 {
		return nil, fmt.Errorf("cannot init: goal already has tasks; use add, drop, or done to modify them")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("cannot init: items must not be empty")
	}
	if len(items) > maxItems {
		return nil, fmt.Errorf("cannot init: too many tasks (max %d)", maxItems)
	}

	tasks := make([]session.Task, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	now := time.Now().Unix()
	for _, content := range items {
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		key := strings.ToLower(content)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("cannot init: duplicate task %q", content)
		}
		seen[key] = struct{}{}
		tasks = append(tasks, session.Task{
			ID:        content,
			Content:   content,
			Status:    session.TaskStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("cannot init: all items were empty")
	}
	return tasks, nil
}

func todoAdd(goal session.Goal, items []string, maxItems int) ([]session.Task, error) {
	if len(goal.Tasks) == 0 {
		return nil, fmt.Errorf("cannot add: no existing tasks; use init first")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("cannot add: items must not be empty")
	}
	if len(goal.Tasks)+len(items) > maxItems {
		return nil, fmt.Errorf("cannot add: would exceed max tasks (%d)", maxItems)
	}

	existing := make(map[string]struct{}, len(goal.Tasks))
	for _, t := range goal.Tasks {
		existing[strings.ToLower(t.Content)] = struct{}{}
	}

	now := time.Now().Unix()
	for _, content := range items {
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		if _, ok := existing[strings.ToLower(content)]; ok {
			return nil, fmt.Errorf("cannot add: duplicate task %q", content)
		}
		goal.Tasks = append(goal.Tasks, session.Task{
			ID:        content,
			Content:   content,
			Status:    session.TaskStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		})
		existing[strings.ToLower(content)] = struct{}{}
	}

	return goal.Tasks, nil
}

func todoStart(goal session.Goal, index int, content string) ([]session.Task, error) {
	i, err := resolveTaskIndex(goal.Tasks, index, content)
	if err != nil {
		return nil, err
	}

	tasks := slices.Clone(goal.Tasks)
	for j := range tasks {
		if j == i {
			tasks[j].Status = session.TaskStatusInProgress
			tasks[j].UpdatedAt = time.Now().Unix()
		} else if tasks[j].Status == session.TaskStatusInProgress {
			tasks[j].Status = session.TaskStatusPending
			tasks[j].UpdatedAt = time.Now().Unix()
		}
	}
	return tasks, nil
}

func todoDone(goal session.Goal, index int, content, evidence string) ([]session.Task, error) {
	i, err := resolveTaskIndex(goal.Tasks, index, content)
	if err != nil {
		return nil, err
	}

	tasks := slices.Clone(goal.Tasks)
	t := &tasks[i]
	now := time.Now().Unix()
	wasCompleted := t.Status == session.TaskStatusCompleted
	t.Status = session.TaskStatusCompleted
	if !wasCompleted {
		t.CompletedAt = now
	}
	t.UpdatedAt = now
	if evidence != "" {
		t.Evidence = strings.TrimSpace(evidence)
	}

	return tasks, nil
}

func todoBlock(goal session.Goal, index int, content, reason string) ([]session.Task, error) {
	if reason == "" {
		return nil, fmt.Errorf("block requires a reason")
	}
	i, err := resolveTaskIndex(goal.Tasks, index, content)
	if err != nil {
		return nil, err
	}

	tasks := slices.Clone(goal.Tasks)
	tasks[i].Status = session.TaskStatusBlocked
	tasks[i].Blocker = strings.TrimSpace(reason)
	tasks[i].UpdatedAt = time.Now().Unix()
	return tasks, nil
}

func todoUnblock(goal session.Goal, index int, content string) ([]session.Task, error) {
	i, err := resolveTaskIndex(goal.Tasks, index, content)
	if err != nil {
		return nil, err
	}

	tasks := slices.Clone(goal.Tasks)
	if tasks[i].Status != session.TaskStatusBlocked {
		return nil, fmt.Errorf("cannot unblock: task %q is not blocked", tasks[i].Content)
	}
	tasks[i].Status = session.TaskStatusPending
	tasks[i].Blocker = ""
	tasks[i].UpdatedAt = time.Now().Unix()
	return tasks, nil
}

func todoDrop(goal session.Goal, index int, content, reason string) ([]session.Task, error) {
	if reason == "" {
		return nil, fmt.Errorf("drop requires a reason")
	}
	i, err := resolveTaskIndex(goal.Tasks, index, content)
	if err != nil {
		return nil, err
	}

	task := goal.Tasks[i]
	if task.Status == session.TaskStatusCompleted {
		return nil, fmt.Errorf("cannot drop a completed task; clear its evidence first if you want to remove it")
	}
	if task.Status == session.TaskStatusDropped {
		return nil, fmt.Errorf("task is already dropped")
	}

	tasks := slices.Clone(goal.Tasks)
	tasks[i].Status = session.TaskStatusDropped
	tasks[i].DropReason = strings.TrimSpace(reason)
	tasks[i].UpdatedAt = time.Now().Unix()
	tasks[i].CompletedAt = 0
	return tasks, nil
}

func resolveTaskIndex(tasks []session.Task, index int, content string) (int, error) {
	if len(tasks) == 0 {
		return -1, fmt.Errorf("no tasks available")
	}
	if content != "" {
		content = strings.ToLower(strings.TrimSpace(content))
		for i, t := range tasks {
			if strings.ToLower(t.Content) == content {
				return i, nil
			}
		}
		return -1, fmt.Errorf("no task found with content %q", content)
	}
	if index < 0 || index >= len(tasks) {
		return -1, fmt.Errorf("task index %d out of range", index)
	}
	return index, nil
}

// handleSubagentTodo restricts todo operations from subagents to safe,
// read-only or queued writes. Subagents cannot directly mutate the parent
// session; start/done updates are queued and applied by the parent agent on its
// next turn.
func handleSubagentTodo(ctx context.Context, sessions session.Service, goalRuntime *goal.Runtime, params TodoParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	parentSessionID := GetParentSessionIDFromContext(ctx)
	if parentSessionID == "" {
		parentSessionID = GetSessionFromContext(ctx)
	}
	if parentSessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("todo tool in subagent mode requires a parent session context")
	}
	if goalRuntime == nil {
		return fantasy.ToolResponse{}, fmt.Errorf("todo tool is not available in this subagent context")
	}

	switch params.Op {
	case "view":
		sess, err := sessions.Get(ctx, parentSessionID)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to get parent session: %w", err)
		}
		if !sess.Goal.IsActive() {
			return fantasy.ToolResponse{}, fmt.Errorf("parent session has no active goal")
		}
		return todoViewResponse(sess.Goal.Tasks), nil

	case "start", "done":
		if call.ID == "" {
			return fantasy.ToolResponse{}, fmt.Errorf("todo %s in subagent mode requires a tool call ID", params.Op)
		}

		var status session.TaskStatus
		var evidence, reason string
		switch params.Op {
		case "start":
			status = session.TaskStatusInProgress
		case "done":
			status = session.TaskStatusCompleted
			evidence = params.Evidence
			if evidence == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("todo done from a subagent requires evidence")
			}
		}

		content := strings.TrimSpace(params.Content)
		if content == "" {
			return fantasy.ToolResponse{}, fmt.Errorf("subagent todo %s requires a task content (the task's text or ID)", params.Op)
		}

		update := goal.SubagentTaskUpdate{
			TaskContent: content,
			NewStatus:   status,
			Evidence:    evidence,
			Reason:      reason,
			ToolCallID:  call.ID,
			Timestamp:   time.Now().Unix(),
		}
		if err := goalRuntime.ApplySubagentTaskUpdate(parentSessionID, update); err != nil {
			return fantasy.ToolResponse{}, err
		}

		metadata, err := json.Marshal(TodoResponseMetadata{Tasks: nil})
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		return fantasy.ToolResponse{
			Content:  fmt.Sprintf("Task update submitted for '%s'. The parent agent will apply it on its next turn.", content),
			Metadata: string(metadata),
		}, nil

	case "init", "add", "block", "unblock", "drop":
		return fantasy.ToolResponse{}, fmt.Errorf("todo %s is not allowed from a subagent", params.Op)
	default:
		return fantasy.ToolResponse{}, fmt.Errorf("unknown todo operation: %s", params.Op)
	}
}

func todoViewResponse(tasks []session.Task) fantasy.ToolResponse {
	text := formatTodoResponse(tasks, "view")
	metadata, err := json.Marshal(TodoResponseMetadata{Tasks: tasks})
	if err != nil {
		return fantasy.ToolResponse{Content: text, IsError: true}
	}
	return fantasy.ToolResponse{Content: text, Metadata: string(metadata)}
}

func formatTodoResponse(tasks []session.Task, op string) string {
	var sb strings.Builder
	switch op {
	case "init":
		sb.WriteString("Task list initialized.\n")
	case "add":
		sb.WriteString("Task added.\n")
	case "start":
		sb.WriteString("Task started.\n")
	case "done":
		sb.WriteString("Task completed.\n")
	case "block":
		sb.WriteString("Task blocked.\n")
	case "unblock":
		sb.WriteString("Task unblocked.\n")
	case "drop":
		sb.WriteString("Task dropped.\n")
	case "view":
		sb.WriteString("Current goal tasks:\n")
	}

	if len(tasks) == 0 {
		sb.WriteString("No tasks.\n")
		return sb.String()
	}

	for i, t := range tasks {
		if t.Evidence != "" {
			fmt.Fprintf(&sb, "%d. [%s] %s (evidence: %s)\n", i+1, t.Status, t.Content, t.Evidence)
		} else if t.Blocker != "" {
			fmt.Fprintf(&sb, "%d. [%s] %s (blocker: %s)\n", i+1, t.Status, t.Content, t.Blocker)
		} else if t.DropReason != "" {
			fmt.Fprintf(&sb, "%d. [%s] %s (drop reason: %s)\n", i+1, t.Status, t.Content, t.DropReason)
		} else {
			fmt.Fprintf(&sb, "%d. [%s] %s\n", i+1, t.Status, t.Content)
		}
	}
	return sb.String()
}
