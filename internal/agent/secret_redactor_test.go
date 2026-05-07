package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactSecrets(t *testing.T) {
	t.Parallel()

	// stripeKey builds a Stripe-format key from parts to avoid triggering
	// secret-scanning on the literal string in source code.
	stripeKey := func(env, suffix string) string {
		return "sk_" + env + "_" + suffix
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "OpenAI key",
			input: "key=sk-abcdefghijklmnopqrstuvwxyz123456",
			want:  "key=[REDACTED]",
		},
		{
			name:  "OpenAI proj key",
			input: "export OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz12345",
			want:  "export OPENAI_API_KEY=[REDACTED]",
		},
		{
			name:  "Anthropic key",
			input: "key=sk-ant-abcdefghijklmnopqrstuvwxyz12345",
			want:  "key=[REDACTED]",
		},
		{
			name:  "HuggingFace token",
			input: "token=hf_abcdefghijklmnopqrstuvwxyz12345678",
			want:  "token=[REDACTED]",
		},
		{
			name:  "GitHub classic PAT",
			input: "ghp_abcdefghijklmnopqrstuvwxyz123456789012",
			want:  "[REDACTED]",
		},
		{
			name:  "GitHub fine-grained PAT",
			input: "github_pat_abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGH",
			want:  "[REDACTED]",
		},
		{
			name:  "AWS access key ID",
			input: "aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
			want:  "aws_access_key_id = [REDACTED]",
		},
		{
			name:  "Google API key",
			input: "key=AIzaxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			want:  "key=[REDACTED]",
		},
		{
			name:  "Stripe live key",
			input: "stripe=" + stripeKey("live", "abcdefghijklmnopqrstuvwx"),
			want:  "stripe=[REDACTED]",
		},
		{
			name:  "Stripe test key",
			input: "stripe=" + stripeKey("test", "abcdefghijklmnopqrstuvwx"),
			want:  "stripe=[REDACTED]",
		},
		{
			name:  "Authorization Bearer header not redacted (user may share for analysis)",
			input: "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature",
			want:  "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature",
		},
		{
			name:  "X-Api-Key header not redacted (user may share for analysis)",
			input: "X-Api-Key: someapikey123456",
			want:  "X-Api-Key: someapikey123456",
		},
		{
			name:  "no secrets",
			input: "Hello, world! This is safe text.",
			want:  "Hello, world! This is safe text.",
		},
		{
			name:  "mixed content",
			input: "token sk-abcdefghijklmnopqrstuvwxyz123456 is set",
			want:  "token [REDACTED] is set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactSecrets(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}
