package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"

	_ "github.com/joho/godotenv/autoload"
)

// fakeEnv is an environment for testing.
type fakeEnv struct {
	workingDir  string
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker *filetracker.Service
	lspClients  *csync.Map[string, *lsp.Client]
}


const emptyTodoReminder = `<system_reminder>This is a reminder that your todo list is currently empty. DO NOT mention this to the user explicitly because they are already aware.
If you are working on tasks that would benefit from a todo list please use the "todos" tool to create one.
If not, please feel free to ignore. Again do not mention this message to the user.</system_reminder>`

type testTodoReminderPlugin struct{}

func (p *testTodoReminderPlugin) Name() string { return "test-todo-reminder" }

func (p *testTodoReminderPlugin) Init(context.Context, plugin.PluginInput) (plugin.Hooks, error) {
	return plugin.Hooks{
		ChatMessagesTransform: func(_ context.Context, _ plugin.ChatMessagesTransformInput, output *plugin.ChatMessagesTransformOutput) error {
			output.Messages = append(output.Messages, message.Message{
				Role: message.User,
				Parts: []message.ContentPart{
					message.TextContent{Text: emptyTodoReminder},
				},
			})
			return nil
		},
	}, nil
}

func (p *testTodoReminderPlugin) Close(context.Context) error { return nil }


func testEnv(t *testing.T) fakeEnv {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	// Use a relative path for cross-platform compatibility
	// This avoids platform-specific absolute paths in cassettes
	workingDir := filepath.Join("crush-test-tmp", t.Name())
	os.RemoveAll(workingDir)

	err := os.MkdirAll(workingDir, 0o755)
	require.NoError(t, err)

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	permissions := permission.NewPermissionService(workingDir, true, []string{})
	history := history.NewService(q, conn)
	filetrackerService := filetracker.NewService(q)
	lspClients := csync.NewMap[string, *lsp.Client]()

	t.Cleanup(func() {
		conn.Close()
		os.RemoveAll(workingDir)
	})

	return fakeEnv{
		workingDir,
		sessions,
		messages,
		permissions,
		history,
		&filetrackerService,
		lspClients,
	}
}

func testSessionAgent(env fakeEnv, large, small fantasy.LanguageModel, systemPrompt string, tools ...fantasy.AgentTool) SessionAgent {
	largeModel := Model{
		Model: large,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	smallModel := Model{
		Model: small,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:   largeModel,
		SmallModel:   smallModel,
		SystemPrompt: systemPrompt,
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		Tools:        tools,
	})
	return agent
}

func createSimpleGoProject(t *testing.T, dir string) {
	goMod := `module example.com/testproject

go 1.23
`
	err := os.WriteFile(dir+"/go.mod", []byte(goMod), 0o644)
	require.NoError(t, err)

	mainGo := `package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
}
`
	err = os.WriteFile(dir+"/main.go", []byte(mainGo), 0o644)
	require.NoError(t, err)
}
