package config

import (
	"testing"
)

func TestParseConfigBytesRemoteManagementBaseURL(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`remote-management:
  allow-remote: true
  base-url: "https://proxy.example.com"
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}

	if cfg.RemoteManagement.BaseURL != "https://proxy.example.com" {
		t.Fatalf("RemoteManagement.BaseURL = %q, want https://proxy.example.com", cfg.RemoteManagement.BaseURL)
	}
}

func TestParseConfigBytesPanelGitHubRepository(t *testing.T) {
	const upstreamRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"

	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing uses fork default",
			yaml: "management:\n  allow-remote: false\n",
			want: DefaultPanelGitHubRepository,
		},
		{
			name: "blank uses fork default",
			yaml: "management:\n  panel-github-repository: \"  \"\n",
			want: DefaultPanelGitHubRepository,
		},
		{
			name: "explicit upstream is preserved",
			yaml: "management:\n  panel-github-repository: \"" + upstreamRepository + "\"\n",
			want: upstreamRepository,
		},
		{
			name: "explicit upstream in legacy section is preserved",
			yaml: "remote-management:\n  panel-github-repository: \"" + upstreamRepository + "\"\n",
			want: upstreamRepository,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if cfg.RemoteManagement.PanelGitHubRepository != tt.want {
				t.Fatalf("RemoteManagement.PanelGitHubRepository = %q, want %q", cfg.RemoteManagement.PanelGitHubRepository, tt.want)
			}
		})
	}

	if DefaultPanelGitHubRepository == upstreamRepository {
		t.Fatal("DefaultPanelGitHubRepository must point at the fork's panel repository")
	}
}
