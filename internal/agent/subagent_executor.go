package agent

import (
	"context"
	"sync"

	"charm.land/fantasy"
)

// executorResult holds the outcome of a single subagent task execution.
type executorResult struct {
	Response fantasy.ToolResponse
	Err      error
}

// subagentExecutor runs subagent tasks in parallel with concurrency limiting.
// It replaces the former TaskGraph DAG orchestration with a simpler model
// where the parent LLM is responsible for all orchestration decisions.
type subagentExecutor struct {
	maxConcurrency int
	failFast       bool
	runOne         func(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error)
}

// newSubagentExecutor creates a new executor with the given concurrency limit.
// If maxConcurrency is <= 0, it defaults to 4. The runner function is called
// for each task and should execute the subagent to completion.
func newSubagentExecutor(
	maxConcurrency int,
	failFast bool,
	runner func(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error),
) *subagentExecutor {
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	return &subagentExecutor{
		maxConcurrency: maxConcurrency,
		failFast:       failFast,
		runOne:         runner,
	}
}

// execute runs all tasks in parallel, bounded by the concurrency semaphore.
// Results are returned in the same order as the input tasks.
// If failFast is true and any task fails, remaining tasks are cancelled.
func (e *subagentExecutor) execute(ctx context.Context, tasks []subAgentParams) []executorResult {
	if len(tasks) == 0 {
		return nil
	}

	results := make([]executorResult, len(tasks))

	// Create a derived context so we can cancel remaining tasks on fail-fast.
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()

	sem := make(chan struct{}, e.maxConcurrency)
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		go func(idx int, params subAgentParams) {
			defer wg.Done()

			// Acquire semaphore slot.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-execCtx.Done():
				results[idx] = executorResult{Err: execCtx.Err()}
				return
			}

			// Check for cancellation before running.
			if err := execCtx.Err(); err != nil {
				results[idx] = executorResult{Err: err}
				return
			}

			resp, err := e.runOne(execCtx, params)
			results[idx] = executorResult{Response: resp, Err: err}

			// If fail-fast is enabled and this task errored, cancel the rest.
			if e.failFast && err != nil {
				execCancel()
			}
		}(i, task)
	}

	wg.Wait()
	return results
}
