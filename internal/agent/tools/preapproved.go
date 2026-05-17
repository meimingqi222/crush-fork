package tools

import (
	"net/url"
	"strings"
)

// PreapprovedHosts is a curated list of code-related domains that are safe
// to access without manual approval in Auto Mode. These domains typically
// host programming documentation, package registries, and developer resources.
//
// This list is inspired by claude-code's WebFetchTool preapproved hosts.
//
// SECURITY NOTE: This list is ONLY for read-only GET requests (read_url,
// download, agentic_fetch). It must NOT be used to grant unrestricted
// network access for mutating operations.
var preapprovedHosts = map[string][]string{
	// Anthropic / MCP
	"platform.claude.com":     nil,
	"code.claude.com":         nil,
	"modelcontextprotocol.io": nil,
	"agentskills.io":          nil,
	"github.com":              {"/anthropics"},

	// Top Programming Languages
	"docs.python.org":        nil,
	"en.cppreference.com":    nil,
	"docs.oracle.com":        nil,
	"learn.microsoft.com":    nil,
	"developer.mozilla.org":  nil,
	"go.dev":                 nil,
	"pkg.go.dev":             nil,
	"www.php.net":            nil,
	"docs.swift.org":         nil,
	"kotlinlang.org":         nil,
	"ruby-doc.org":           nil,
	"doc.rust-lang.org":      nil,
	"www.typescriptlang.org": nil,

	// Web & JavaScript Frameworks/Libraries
	"react.dev":        nil,
	"angular.io":       nil,
	"vuejs.org":        nil,
	"nextjs.org":       nil,
	"expressjs.com":    nil,
	"nodejs.org":       nil,
	"bun.sh":           nil,
	"jquery.com":       nil,
	"getbootstrap.com": nil,
	"tailwindcss.com":  nil,
	"d3js.org":         nil,
	"threejs.org":      nil,
	"redux.js.org":     nil,
	"webpack.js.org":   nil,
	"jestjs.io":        nil,
	"reactrouter.com":  nil,

	// Python Frameworks & Libraries
	"docs.djangoproject.com":    nil,
	"flask.palletsprojects.com": nil,
	"fastapi.tiangolo.com":      nil,
	"pandas.pydata.org":         nil,
	"numpy.org":                 nil,
	"www.tensorflow.org":        nil,
	"pytorch.org":               nil,
	"scikit-learn.org":          nil,
	"matplotlib.org":            nil,
	"requests.readthedocs.io":   nil,
	"jupyter.org":               nil,

	// PHP Frameworks
	"laravel.com":   nil,
	"symfony.com":   nil,
	"wordpress.org": nil,

	// Java Frameworks & Libraries
	"docs.spring.io":    nil,
	"hibernate.org":     nil,
	"tomcat.apache.org": nil,
	"gradle.org":        nil,
	"maven.apache.org":  nil,

	// .NET & C# Frameworks
	"asp.net":              nil,
	"dotnet.microsoft.com": nil,
	"nuget.org":            nil,
	"blazor.net":           nil,

	// Mobile Development
	"reactnative.dev":       nil,
	"docs.flutter.dev":      nil,
	"developer.apple.com":   nil,
	"developer.android.com": nil,

	// Data Science & Machine Learning
	"keras.io":         nil,
	"spark.apache.org": nil,
	"huggingface.co":   nil,
	"www.kaggle.com":   nil,

	// Databases
	"www.mongodb.com":    nil,
	"redis.io":           nil,
	"www.postgresql.org": nil,
	"dev.mysql.com":      nil,
	"www.sqlite.org":     nil,
	"graphql.org":        nil,
	"prisma.io":          nil,

	// Cloud & DevOps
	"docs.aws.amazon.com":  nil,
	"cloud.google.com":     nil,
	"kubernetes.io":        nil,
	"www.docker.com":       nil,
	"www.terraform.io":     nil,
	"www.ansible.com":      nil,
	"vercel.com":           {"/docs"},
	"docs.netlify.com":     nil,
	"devcenter.heroku.com": nil,

	// Testing & Monitoring
	"cypress.io":   nil,
	"selenium.dev": nil,

	// Game Development
	"docs.unity.com":        nil,
	"docs.unrealengine.com": nil,

	// Other Essential Tools
	"git-scm.com":      nil,
	"nginx.org":        nil,
	"httpd.apache.org": nil,
}

// IsPreapprovedHost reports whether the given hostname and pathname are in
// the preapproved list. If pathname is empty, only the hostname is checked.
func IsPreapprovedHost(hostname, pathname string) bool {
	prefixes, ok := preapprovedHosts[hostname]
	if !ok {
		return false
	}
	if prefixes == nil {
		return true
	}
	for _, p := range prefixes {
		if pathname == p || strings.HasPrefix(pathname, p+"/") {
			return true
		}
	}
	return false
}

// ExtractURLFromPermissionRequest attempts to extract a URL from a permission
// request's params. It supports the params structures used by read_url,
// download, and agentic_fetch actions.
func ExtractURLFromPermissionRequest(params any) string {
	switch p := params.(type) {
	case ReadPermissionsParams:
		return p.Path
	case DownloadPermissionsParams:
		return p.URL
	case AgenticFetchPermissionsParams:
		return p.URL
	case map[string]any:
		// Fallback for generic param maps (e.g. from plugin hooks).
		for _, key := range []string{"url", "URL", "path", "Path"} {
			if v, ok := p[key]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// IsPreapprovedURL parses a URL string and reports whether its host (and
// optional path prefix) is in the preapproved list.
func IsPreapprovedURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return IsPreapprovedHost(u.Hostname(), u.Path)
}
