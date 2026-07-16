package tools

import (
	"context"

	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
)

type mockPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockPermissionService) EvaluateRequest(context.Context, permission.CreatePermissionRequest) (permission.EvaluationResult, error) {
	return permission.EvaluationResult{Decision: permission.EvaluationDecisionAllow}, nil
}

func (m *mockPermissionService) Prompt(context.Context, permission.PermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockPermissionService) Grant(req permission.PermissionRequest) {}

func (m *mockPermissionService) Deny(req permission.PermissionRequest) {}

func (m *mockPermissionService) GrantPersistent(req permission.PermissionRequest) {}

func (m *mockPermissionService) HasPersistentPermission(permission.PermissionRequest) bool {
	return false
}

func (m *mockPermissionService) ClearPersistentPermissions(string) {}

func (m *mockPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockPermissionService) SetSessionAutoApprove(sessionID string, enabled bool) {}

func (m *mockPermissionService) IsSessionAutoApprove(sessionID string) bool {
	return false
}

func (m *mockPermissionService) SetSkipRequests(skip bool) {}

func (m *mockPermissionService) SkipRequests() bool {
	return false
}

func (m *mockPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func (m *mockPermissionService) SetSkillContext(string, []string) {}
func (m *mockPermissionService) ClearSkillContext()               {}
func (m *mockPermissionService) GetSkillContext() (string, []string) {
	return "", nil
}

type mockHistoryService struct {
	*pubsub.Broker[history.File]
}

func (m *mockHistoryService) Create(ctx context.Context, sessionID, path, content string) (history.File, error) {
	return history.File{Path: path, Content: content}, nil
}

func (m *mockHistoryService) CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error) {
	return history.File{}, nil
}

func (m *mockHistoryService) GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error) {
	return history.File{Path: path, Content: ""}, nil
}

func (m *mockHistoryService) Get(ctx context.Context, id string) (history.File, error) {
	return history.File{}, nil
}

func (m *mockHistoryService) ListBySession(ctx context.Context, sessionID string) ([]history.File, error) {
	return nil, nil
}

func (m *mockHistoryService) ListLatestSessionFiles(ctx context.Context, sessionID string) ([]history.File, error) {
	return nil, nil
}

func (m *mockHistoryService) SearchMessages(ctx context.Context, params history.SearchParams) ([]history.MessageSearchResult, error) {
	return nil, nil
}

func (m *mockHistoryService) SearchMessagesPage(ctx context.Context, params history.SearchParams) ([]history.MessageSearchResult, error) {
	return m.SearchMessages(ctx, params)
}

func (m *mockHistoryService) Delete(ctx context.Context, id string) error {
	return nil
}

func (m *mockHistoryService) DeleteSessionFiles(ctx context.Context, sessionID string) error {
	return nil
}
