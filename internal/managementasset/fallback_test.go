package managementasset

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

const (
	upstreamPanelRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	forkPanelRepository     = "https://github.com/mbartelt-bit/Cli-Proxy-API-Management-Center"
	fallbackConnectHost     = "cpamc.router-for.me:443"
	githubAPIConnectHost    = "api.github.com:443"
)

func TestDefaultReleaseURLFollowsDefaultPanelRepository(t *testing.T) {
	if got := resolveReleaseURL(config.DefaultPanelGitHubRepository); got != defaultManagementReleaseURL {
		t.Fatalf("resolveReleaseURL(DefaultPanelGitHubRepository) = %q, want %q", got, defaultManagementReleaseURL)
	}
	if isUpstreamManagementReleaseURL(defaultManagementReleaseURL) {
		t.Fatal("default release URL must not be treated as the upstream panel repository")
	}
	if !isUpstreamManagementReleaseURL(resolveReleaseURL(upstreamPanelRepository)) {
		t.Fatal("upstream panel repository must resolve to the upstream release URL")
	}
}

func TestIsUpstreamManagementReleaseURL(t *testing.T) {
	tests := []struct {
		name string
		repo string
		want bool
	}{
		{name: "upstream repository URL", repo: upstreamPanelRepository, want: true},
		{name: "upstream trailing slash", repo: upstreamPanelRepository + "/", want: true},
		{name: "upstream git suffix", repo: upstreamPanelRepository + ".git", want: true},
		{name: "upstream surrounding whitespace", repo: "  " + upstreamPanelRepository + "  ", want: true},
		{name: "upstream mixed case", repo: "https://GitHub.com/Router-For-Me/cli-proxy-api-management-center", want: true},
		{name: "upstream releases page", repo: upstreamPanelRepository + "/releases", want: true},
		{name: "upstream API repository URL", repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream API releases URL", repo: upstreamManagementReleaseURL, want: true},
		{name: "upstream API releases URL trailing slash", repo: upstreamManagementReleaseURL + "/", want: true},
		{name: "fork repository URL", repo: forkPanelRepository, want: false},
		{name: "fork API releases URL", repo: defaultManagementReleaseURL, want: false},
		{name: "empty uses fork default", repo: "", want: false},
		{name: "other owner", repo: "https://github.com/someone/Cli-Proxy-API-Management-Center", want: false},
		{name: "non GitHub host", repo: "https://example.com/router-for-me/Cli-Proxy-API-Management-Center", want: false},
		{name: "unparseable", repo: "://not a url", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			releaseURL := resolveReleaseURL(tt.repo)
			if got := isUpstreamManagementReleaseURL(releaseURL); got != tt.want {
				t.Fatalf("isUpstreamManagementReleaseURL(resolveReleaseURL(%q) = %q) = %t, want %t", tt.repo, releaseURL, got, tt.want)
			}
		})
	}
}

func TestEnsureFallbackManagementHTMLSkipsNonUpstreamRepository(t *testing.T) {
	tests := []struct {
		name          string
		existingAsset string
	}{
		{name: "local asset missing"},
		{name: "local asset present", existingAsset: "<html>fork panel</html>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := captureWarnings(t)
			localPath := filepath.Join(t.TempDir(), managementAssetName)
			if tt.existingAsset != "" {
				writeAsset(t, localPath, tt.existingAsset)
			}

			client := &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
				t.Errorf("unexpected request to %s", req.URL)
				return nil, errors.New("unexpected request")
			})}

			if ensureFallbackManagementHTML(t.Context(), client, localPath, resolveReleaseURL(forkPanelRepository)) {
				t.Fatal("ensureFallbackManagementHTML() = true, want false for a non-upstream repository")
			}

			if tt.existingAsset == "" {
				if _, errStat := os.Stat(localPath); !errors.Is(errStat, os.ErrNotExist) {
					t.Fatalf("local asset stat error = %v, want not exist", errStat)
				}
			} else if got := readAsset(t, localPath); got != tt.existingAsset {
				t.Fatalf("local asset = %q, want unchanged %q", got, tt.existingAsset)
			}

			if !hasWarningContaining(hook, "skipping fallback management page") {
				t.Fatal("expected a warning explaining why the fallback page was skipped")
			}
		})
	}
}

func TestEnsureFallbackManagementHTMLKeepsUpstreamBehaviour(t *testing.T) {
	const fallbackBody = "<html>upstream hosted panel</html>"

	var requested []string
	client := &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.String())
		if req.URL.String() != defaultManagementFallbackURL {
			return nil, errors.New("unexpected request")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fallbackBody))}, nil
	})}

	localPath := filepath.Join(t.TempDir(), managementAssetName)
	if !ensureFallbackManagementHTML(t.Context(), client, localPath, resolveReleaseURL(upstreamPanelRepository+".git")) {
		t.Fatal("ensureFallbackManagementHTML() = false, want true for the upstream repository")
	}
	if !slices.Equal(requested, []string{defaultManagementFallbackURL}) {
		t.Fatalf("requested = %v, want only %q", requested, defaultManagementFallbackURL)
	}
	if got := readAsset(t, localPath); got != fallbackBody {
		t.Fatalf("local asset = %q, want fallback body %q", got, fallbackBody)
	}
}

// TestEnsureLatestManagementHTMLFallbackGate drives the full sync path through an HTTP proxy that
// refuses every tunnel, which makes the GitHub release lookup fail without touching the network and
// records which hosts the updater tried to reach.
func TestEnsureLatestManagementHTMLFallbackGate(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("github_token", "")
	t.Setenv("GITSTORE_GIT_TOKEN", "")
	t.Setenv("GITSTORE_GIT_URL", "")

	tests := []struct {
		name          string
		repository    string
		existingAsset string
		wantHosts     []string
		wantAsset     bool
	}{
		{
			name:       "default fork repository with missing asset skips fallback",
			repository: config.DefaultPanelGitHubRepository,
			wantHosts:  []string{githubAPIConnectHost},
		},
		{
			name:       "empty repository uses fork default and skips fallback",
			repository: "",
			wantHosts:  []string{githubAPIConnectHost},
		},
		{
			name:          "fork repository keeps existing asset",
			repository:    forkPanelRepository,
			existingAsset: "<html>fork panel</html>",
			wantHosts:     []string{githubAPIConnectHost},
			wantAsset:     true,
		},
		{
			name:       "upstream repository still tries fallback",
			repository: upstreamPanelRepository + "/",
			wantHosts:  []string{githubAPIConnectHost, fallbackConnectHost},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := newRefusingProxy(t)
			resetSyncThrottle(t)

			staticDir := t.TempDir()
			localPath := filepath.Join(staticDir, managementAssetName)
			if tt.existingAsset != "" {
				writeAsset(t, localPath, tt.existingAsset)
			}

			got := EnsureLatestManagementHTML(t.Context(), staticDir, proxy.url, tt.repository)
			if got != tt.wantAsset {
				t.Fatalf("EnsureLatestManagementHTML() = %t, want %t", got, tt.wantAsset)
			}
			if hosts := proxy.connectHosts(); !slices.Equal(hosts, tt.wantHosts) {
				t.Fatalf("CONNECT hosts = %v, want %v", hosts, tt.wantHosts)
			}
			if tt.existingAsset != "" {
				if asset := readAsset(t, localPath); asset != tt.existingAsset {
					t.Fatalf("local asset = %q, want unchanged %q", asset, tt.existingAsset)
				}
			}
		})
	}
}

type refusingProxy struct {
	url   string
	mu    sync.Mutex
	hosts []string
}

func newRefusingProxy(t *testing.T) *refusingProxy {
	t.Helper()
	proxy := &refusingProxy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.mu.Lock()
		proxy.hosts = append(proxy.hosts, req.Host)
		proxy.mu.Unlock()
		http.Error(w, "tunnel refused", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	proxy.url = server.URL
	return proxy
}

func (p *refusingProxy) connectHosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.hosts)
}

// resetSyncThrottle clears the package-level sync throttle so consecutive cases are not skipped.
func resetSyncThrottle(t *testing.T) {
	t.Helper()
	reset := func() {
		lastUpdateCheckMu.Lock()
		lastUpdateCheckTime = time.Time{}
		lastUpdateCheckMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func captureWarnings(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
	})
	return hook
}

func hasWarningContaining(hook *logtest.Hook, substring string) bool {
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, substring) {
			return true
		}
	}
	return false
}

func writeAsset(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write local asset: %v", err)
	}
}

func readAsset(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read local asset: %v", err)
	}
	return string(data)
}
