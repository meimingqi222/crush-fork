package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsPreapprovedHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hostname string
		pathname string
		want     bool
	}{
		{
			name:     "exact hostname match",
			hostname: "docs.python.org",
			pathname: "/3/library/os.html",
			want:     true,
		},
		{
			name:     "hostname not in list",
			hostname: "evil.com",
			pathname: "/",
			want:     false,
		},
		{
			name:     "github.com without prefix",
			hostname: "github.com",
			pathname: "/some/repo",
			want:     false,
		},
		{
			name:     "github.com with anthropics prefix",
			hostname: "github.com",
			pathname: "/anthropics/claude-code",
			want:     true,
		},
		{
			name:     "github.com with anthropics prefix exact",
			hostname: "github.com",
			pathname: "/anthropics",
			want:     true,
		},
		{
			name:     "vercel.com docs prefix",
			hostname: "vercel.com",
			pathname: "/docs/getting-started",
			want:     true,
		},
		{
			name:     "vercel.com without docs prefix",
			hostname: "vercel.com",
			pathname: "/dashboard",
			want:     false,
		},
		{
			name:     "empty pathname",
			hostname: "go.dev",
			pathname: "",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, IsPreapprovedHost(tt.hostname, tt.pathname))
		})
	}
}

func TestIsPreapprovedURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{
			name: "preapproved docs url",
			url:  "https://docs.python.org/3/library/os.html",
			want: true,
		},
		{
			name: "non-preapproved url",
			url:  "https://evil.com/malware",
			want: false,
		},
		{
			name: "empty url",
			url:  "",
			want: false,
		},
		{
			name: "invalid url",
			url:  "://not-a-url",
			want: false,
		},
		{
			name: "github search url",
			url:  "https://github.com/search?q=test",
			want: false,
		},
		{
			name: "github anthropics url",
			url:  "https://github.com/anthropics/claude-code",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, IsPreapprovedURL(tt.url))
		})
	}
}

func TestExtractURLFromPermissionRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params any
		want   string
	}{
		{
			name:   "ReadPermissionsParams with URL path",
			params: ReadPermissionsParams{Path: "https://docs.python.org/3/library/os.html"},
			want:   "https://docs.python.org/3/library/os.html",
		},
		{
			name:   "DownloadPermissionsParams",
			params: DownloadPermissionsParams{URL: "https://example.com/file.zip"},
			want:   "https://example.com/file.zip",
		},
		{
			name:   "AgenticFetchPermissionsParams",
			params: AgenticFetchPermissionsParams{URL: "https://docs.python.org"},
			want:   "https://docs.python.org",
		},
		{
			name:   "map with url key",
			params: map[string]any{"url": "https://go.dev"},
			want:   "https://go.dev",
		},
		{
			name:   "map with Path key",
			params: map[string]any{"Path": "https://pkg.go.dev"},
			want:   "https://pkg.go.dev",
		},
		{
			name:   "unsupported type",
			params: 42,
			want:   "",
		},
		{
			name:   "empty params",
			params: ReadPermissionsParams{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, ExtractURLFromPermissionRequest(tt.params))
		})
	}
}
