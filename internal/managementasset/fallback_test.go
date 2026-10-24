package managementasset

import (
	"crypto/sha256"
	"encoding/hex"
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
		{name: "upstream several trailing slashes", repo: upstreamPanelRepository + "///", want: true},
		{name: "upstream git suffix", repo: upstreamPanelRepository + ".git", want: true},
		{name: "upstream upper-case git suffix", repo: upstreamPanelRepository + ".GIT", want: true},
		{name: "upstream surrounding whitespace", repo: "  " + upstreamPanelRepository + "  ", want: true},
		{name: "upstream mixed case", repo: "https://GitHub.com/Router-For-Me/cli-proxy-api-management-center", want: true},
		{name: "upstream http scheme", repo: "http://github.com/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream www host", repo: "https://www.github.com/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream explicit port", repo: "https://github.com:443/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream without scheme", repo: "github.com/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream www without scheme", repo: "www.github.com/router-for-me/Cli-Proxy-API-Management-Center/", want: true},
		{name: "upstream owner/repo", repo: "router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream SSH remote", repo: "git@github.com:router-for-me/Cli-Proxy-API-Management-Center.git", want: true},
		{name: "upstream SSH URL", repo: "ssh://git@github.com/router-for-me/Cli-Proxy-API-Management-Center.git", want: true},
		{name: "upstream releases page", repo: upstreamPanelRepository + "/releases", want: true},
		{name: "upstream query and fragment", repo: upstreamPanelRepository + "?tab=readme#top", want: true},
		{name: "upstream API repository URL", repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream API releases list URL", repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases", want: true},
		{name: "upstream API releases URL", repo: upstreamManagementReleaseURL, want: true},
		{name: "upstream API releases URL trailing slash", repo: upstreamManagementReleaseURL + "/", want: true},
		{name: "upstream API http scheme", repo: "http://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest", want: true},
		{name: "upstream API query", repo: upstreamManagementReleaseURL + "?per_page=1", want: true},
		{name: "upstream API credentials", repo: "https://user:secret@api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream API mixed case", repo: "https://API.GitHub.com/Repos/ROUTER-FOR-ME/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream API without scheme", repo: "api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center", want: true},
		{name: "upstream API repository id releases URL", repo: "https://api.github.com/repositories/1051566067/releases/latest", want: true},
		{name: "upstream API repository id releases list URL", repo: "https://api.github.com/repositories/1051566067/releases", want: true},
		{name: "upstream API repository id URL", repo: "https://api.github.com/repositories/1051566067/", want: true},
		{name: "other API repository id", repo: "https://api.github.com/repositories/12345/releases/latest", want: false},
		{name: "non-numeric API repository id", repo: "https://api.github.com/repositories/router-for-me/releases/latest", want: false},
		{name: "API repository id with leading zero", repo: "https://api.github.com/repositories/01051566067/releases/latest", want: true},
		{name: "fork repository URL", repo: forkPanelRepository, want: false},
		{name: "fork owner/repo", repo: "mbartelt-bit/Cli-Proxy-API-Management-Center", want: false},
		{name: "fork API releases URL", repo: defaultManagementReleaseURL, want: false},
		{name: "empty uses fork default", repo: "", want: false},
		{name: "other owner", repo: "https://github.com/someone/Cli-Proxy-API-Management-Center", want: false},
		{name: "other repository name", repo: "https://github.com/router-for-me/CLIProxyAPI", want: false},
		{name: "non GitHub host", repo: "https://example.com/router-for-me/Cli-Proxy-API-Management-Center", want: false},
		{name: "look-alike host", repo: "https://github.com.example.com/router-for-me/Cli-Proxy-API-Management-Center", want: false},
		{name: "owner only", repo: "https://github.com/router-for-me", want: false},
		{name: "API path without repos", repo: "https://api.github.com/router-for-me/Cli-Proxy-API-Management-Center", want: false},
		{name: "unparseable", repo: "://not a url", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captureWarnings(t)
			releaseURL := resolveReleaseURL(tt.repo)
			if got := isUpstreamManagementReleaseURL(releaseURL); got != tt.want {
				t.Fatalf("isUpstreamManagementReleaseURL(resolveReleaseURL(%q) = %q) = %t, want %t", tt.repo, releaseURL, got, tt.want)
			}
		})
	}
}

func TestResolveReleaseURL(t *testing.T) {
	tests := []struct {
		name string
		repo string
		want string
	}{
		{name: "empty", repo: "", want: defaultManagementReleaseURL},
		{name: "default repository", repo: forkPanelRepository, want: defaultManagementReleaseURL},
		{name: "upstream repository", repo: upstreamPanelRepository, want: upstreamManagementReleaseURL},
		{name: "upstream releases page", repo: upstreamPanelRepository + "/releases/tag/v1.2.3", want: upstreamManagementReleaseURL},
		{name: "upstream API releases list URL", repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases", want: upstreamManagementReleaseURL},
		{name: "upstream API http scheme and query", repo: "http://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center?per_page=1", want: upstreamManagementReleaseURL},
		{name: "owner/repo keeps case", repo: "Someone/My-Panel.git", want: "https://api.github.com/repos/Someone/My-Panel/releases/latest"},
		{name: "API credentials are kept", repo: "https://user:secret@api.github.com/repos/someone/private-panel", want: "https://user:secret@api.github.com/repos/someone/private-panel/releases/latest"},
		{name: "github.com credentials are not sent to the API host", repo: "https://user:secret@github.com/someone/private-panel", want: "https://api.github.com/repos/someone/private-panel/releases/latest"},
		{name: "path traversal is not a repository", repo: "https://github.com/someone/..", want: defaultManagementReleaseURL},
		{name: "API repository id", repo: "https://api.github.com/repositories/42", want: "https://api.github.com/repositories/42/releases/latest"},
		{name: "API repository id releases latest URL", repo: "https://api.github.com/repositories/1051566067/releases/latest", want: "https://api.github.com/repositories/1051566067/releases/latest"},
		{name: "API repository id http scheme and query", repo: "http://API.github.com/Repositories/42/releases?per_page=1", want: "https://api.github.com/repositories/42/releases/latest"},
		{name: "API repository id credentials are kept", repo: "https://user:secret@api.github.com/repositories/42", want: "https://user:secret@api.github.com/repositories/42/releases/latest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captureWarnings(t)
			if got := resolveReleaseURL(tt.repo); got != tt.want {
				t.Fatalf("resolveReleaseURL(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}

func TestResolveReleaseURLWarnsWhenRepositoryIsNotRecognized(t *testing.T) {
	tests := []struct {
		name        string
		repo        string
		wantInLog   string
		secretInLog string
	}{
		{name: "unparseable", repo: "://not a url", wantInLog: `"://not a url"`},
		{name: "non GitHub host", repo: "https://gitlab.com/router-for-me/Cli-Proxy-API-Management-Center", wantInLog: `"https://gitlab.com/router-for-me/Cli-Proxy-API-Management-Center"`},
		{name: "credentials and query are not logged", repo: "https://user:hunter2@gitlab.com/o/r?token=abc123", wantInLog: `"https://gitlab.com/o/r"`, secretInLog: "hunter2"},
		{name: "scheme-less credentials are not logged", repo: "user:hunter2@gitlab.com/o/r", wantInLog: `"gitlab.com/o/r"`, secretInLog: "hunter2"},
		{name: "password with # is not logged", repo: "https://deploy:Tr0ub4dor#3@github.com/someone/private-panel", wantInLog: `"` + redactedLogValue + `"`, secretInLog: "Tr0ub4dor"},
		{name: "password with ? is not logged", repo: "https://deploy:Tr0ub4dor?3@github.com/someone/private-panel", wantInLog: `"` + redactedLogValue + `"`, secretInLog: "Tr0ub4dor"},
		{name: "scheme-less password with # is not logged", repo: "deploy:Tr0ub4dor#3@github.com/someone/private-panel", wantInLog: `"` + redactedLogValue + `"`, secretInLog: "Tr0ub4dor"},
		{name: "non GitHub password with # is not logged", repo: "https://user:p#ss@gitlab.com/o/r", wantInLog: `"` + redactedLogValue + `"`, secretInLog: "user:p"},
		{name: "query with an @ is not logged", repo: "https://gitlab.com/o/r?x=a@b&token=QSECRET", wantInLog: `"` + redactedLogValue + `"`, secretInLog: "QSECRET"},
		{name: "API repository id that is not numeric", repo: "https://api.github.com/repositories/abc/releases/latest", wantInLog: `"https://api.github.com/repositories/abc/releases/latest"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := captureWarnings(t)
			if got := resolveReleaseURL(tt.repo); got != defaultManagementReleaseURL {
				t.Fatalf("resolveReleaseURL(%q) = %q, want default %q", tt.repo, got, defaultManagementReleaseURL)
			}
			messages := loggedMessages(hook)
			if len(messages) != 1 {
				t.Fatalf("logged %d warnings, want 1: %q", len(messages), messages)
			}
			for _, want := range []string{"management.panel-github-repository " + tt.wantInLog, "not a recognized GitHub repository", config.DefaultPanelGitHubRepository} {
				if !strings.Contains(messages[0], want) {
					t.Fatalf("warning %q does not contain %q", messages[0], want)
				}
			}
			if strings.Contains(messages[0], "abc123") || (tt.secretInLog != "" && strings.Contains(messages[0], tt.secretInLog)) {
				t.Fatalf("warning %q leaks credentials or query", messages[0])
			}
		})
	}

	t.Run("recognized and empty values do not warn", func(t *testing.T) {
		hook := captureWarnings(t)
		for _, repo := range []string{"", forkPanelRepository, "github.com/router-for-me/Cli-Proxy-API-Management-Center"} {
			resolveReleaseURL(repo)
		}
		if messages := loggedMessages(hook); len(messages) != 0 {
			t.Fatalf("logged warnings %q, want none", messages)
		}
	})
}

// TestSyncManagementAssetFailureLogs pins one coherent log sequence per failure: "trying fallback page" is only
// announced when the fallback is used (the upstream repository with no local copy), and the non-upstream warning
// names the failed stage with a remedy that fits it.
func TestSyncManagementAssetFailureLogs(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("github_token", "")
	t.Setenv("GITSTORE_GIT_TOKEN", "")
	t.Setenv("GITSTORE_GIT_URL", "")

	const (
		releaseBody  = "<html>release panel</html>"
		fallbackBody = "<html>upstream hosted panel</html>"
		localBody    = "<html>local panel</html>"
		downloadURL  = "https://github.com/owner/repo/releases/download/v1.27.0/management.html"
	)
	releaseDigest := sha256.Sum256([]byte(releaseBody))
	releaseJSON := `{"assets":[{"name":"management.html","browser_download_url":"` + downloadURL + `","digest":"sha256:` + hex.EncodeToString(releaseDigest[:]) + `"}]}`
	mismatchJSON := `{"assets":[{"name":"management.html","browser_download_url":"` + downloadURL + `","digest":"sha256:` + strings.Repeat("0", 64) + `"}]}`
	notFound := respond(http.StatusNotFound, `{"message":"Not Found","status":"404"}`)
	releaseOK := respond(http.StatusOK, releaseJSON)
	downloadOK := respond(http.StatusOK, releaseBody)
	refused := func() (*http.Response, error) { return nil, errors.New("connection refused") }

	const forkSkip = "no management panel is available until a sync succeeds (the fallback page https://cpamc.router-for.me/ is the upstream panel build, so it is not used for mbartelt-bit/Cli-Proxy-API-Management-Center)"
	publishHint := []string{"GitHub reports no published release with management.html for mbartelt-bit/Cli-Proxy-API-Management-Center",
		"enable GitHub Actions at https://github.com/mbartelt-bit/Cli-Proxy-API-Management-Center/actions", "vX.Y.Z tag that does not already exist", upstreamPanelRepository}

	tests := []struct {
		name          string
		repository    string
		existingAsset string
		release       func() (*http.Response, error)
		download      func() (*http.Response, error)
		wantFallback  bool
		wantAsset     string
		wantLogs      [][]string // one entry per logged warning or error, each listing required substrings
		wantNot       []string
	}{
		{
			name:       "fork release missing",
			repository: forkPanelRepository,
			release:    notFound,
			wantLogs:   [][]string{append([]string{"failed to fetch latest management release information; " + forkSkip}, publishHint...)},
			wantNot:    []string{"trying fallback page", "check network access"},
		},
		{
			name:       "fork release without management.html",
			repository: forkPanelRepository,
			release:    respond(http.StatusOK, `{"assets":[{"name":"other.zip"}]}`),
			wantLogs:   [][]string{append([]string{"failed to fetch latest management release information; " + forkSkip}, publishHint...)},
			wantNot:    []string{"trying fallback page"},
		},
		{
			name:       "fork release lookup unreachable",
			repository: forkPanelRepository,
			release:    refused,
			wantLogs:   [][]string{{"failed to fetch latest management release information; " + forkSkip, "the release lookup is retried on the next sync"}},
			wantNot:    []string{"trying fallback page", "enable GitHub Actions", "publish"},
		},
		{
			name:       "fork asset download failed",
			repository: forkPanelRepository,
			release:    releaseOK,
			download:   respond(http.StatusBadGateway, "bad gateway"),
			wantLogs:   [][]string{{"failed to download management asset; " + forkSkip, "the latest release lists the asset, so check network access and proxy-url"}},
			wantNot:    []string{"trying fallback page", "enable GitHub Actions", "publish"},
		},
		{
			name:       "fork digest mismatch",
			repository: forkPanelRepository,
			release:    respond(http.StatusOK, mismatchJSON),
			download:   downloadOK,
			wantLogs:   [][]string{{"management asset digest mismatch", "aborting update for safety"}},
			wantNot:    []string{"fallback"},
		},
		{
			name:          "fork release missing keeps local copy",
			repository:    forkPanelRepository,
			existingAsset: localBody,
			release:       notFound,
			wantAsset:     localBody,
			wantLogs:      [][]string{{"failed to fetch latest management release information"}},
			wantNot:       []string{"fallback", "no management panel"},
		},
		{
			name:       "repository id release missing",
			repository: "https://api.github.com/repositories/12345",
			release:    notFound,
			wantLogs: [][]string{{"failed to fetch latest management release information; no management panel is available until a sync succeeds",
				"it is not used for repository id 12345)", "no published release with management.html for repository id 12345",
				"enable GitHub Actions at the Actions tab of repository id 12345 ("}},
			wantNot: []string{"trying fallback page", "https://github.com/repository"},
		},
		{
			name:       "fork release published",
			repository: forkPanelRepository,
			release:    releaseOK,
			download:   downloadOK,
			wantAsset:  releaseBody,
		},
		{
			name:         "upstream release missing uses fallback",
			repository:   "github.com/router-for-me/Cli-Proxy-API-Management-Center",
			release:      notFound,
			wantFallback: true,
			wantAsset:    fallbackBody,
			wantLogs: [][]string{
				{"failed to fetch latest management release information, trying fallback page"},
				{"management asset downloaded from fallback URL without digest verification"},
			},
			wantNot: []string{"no management panel", "enable GitHub Actions"},
		},
		{
			name:         "upstream asset download failed uses fallback",
			repository:   upstreamPanelRepository,
			release:      releaseOK,
			download:     refused,
			wantFallback: true,
			wantAsset:    fallbackBody,
			wantLogs: [][]string{
				{"failed to download management asset, trying fallback page"},
				{"management asset downloaded from fallback URL without digest verification"},
			},
		},
		{
			name:       "upstream digest mismatch does not use fallback",
			repository: upstreamPanelRepository,
			release:    respond(http.StatusOK, mismatchJSON),
			download:   downloadOK,
			wantLogs:   [][]string{{"management asset digest mismatch"}},
			wantNot:    []string{"fallback"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := captureWarnings(t)
			releaseURL := resolveReleaseURL(tt.repository)
			localPath := filepath.Join(t.TempDir(), managementAssetName)
			if tt.existingAsset != "" {
				writeAsset(t, localPath, tt.existingAsset)
			}

			fallbackRequested := false
			client := &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
				switch req.URL.String() {
				case releaseURL:
					return tt.release()
				case downloadURL:
					if tt.download == nil {
						t.Errorf("unexpected asset download")
						return nil, errors.New("unexpected request")
					}
					return tt.download()
				case defaultManagementFallbackURL:
					fallbackRequested = true
					return respond(http.StatusOK, fallbackBody)()
				}
				t.Errorf("unexpected request to %s", req.URL)
				return nil, errors.New("unexpected request")
			})}

			syncManagementAsset(t.Context(), client, localPath, releaseURL, tt.existingAsset == "")

			if fallbackRequested != tt.wantFallback {
				t.Fatalf("fallback requested = %t, want %t", fallbackRequested, tt.wantFallback)
			}
			if tt.wantAsset == "" {
				if _, errStat := os.Stat(localPath); !errors.Is(errStat, os.ErrNotExist) {
					t.Fatalf("local asset stat error = %v, want not exist", errStat)
				}
			} else if got := readAsset(t, localPath); got != tt.wantAsset {
				t.Fatalf("local asset = %q, want %q", got, tt.wantAsset)
			}

			messages := loggedMessages(hook)
			if len(messages) != len(tt.wantLogs) {
				t.Fatalf("logged %d warnings/errors, want %d: %q", len(messages), len(tt.wantLogs), messages)
			}
			for i, wants := range tt.wantLogs {
				for _, want := range wants {
					if !strings.Contains(messages[i], want) {
						t.Fatalf("log %d %q does not contain %q", i, messages[i], want)
					}
				}
			}
			for _, message := range messages {
				for _, unwanted := range tt.wantNot {
					if strings.Contains(message, unwanted) {
						t.Fatalf("log %q contains %q", message, unwanted)
					}
				}
			}
		})
	}
}

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
		{
			name:       "scheme-less upstream repository still tries fallback",
			repository: "github.com/router-for-me/Cli-Proxy-API-Management-Center",
			wantHosts:  []string{githubAPIConnectHost, fallbackConnectHost},
		},
		{
			name:       "upstream releases API URL still tries fallback",
			repository: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases",
			wantHosts:  []string{githubAPIConnectHost, fallbackConnectHost},
		},
		{
			name:       "upstream repository id API URL still tries fallback",
			repository: "https://api.github.com/repositories/1051566067/releases/latest",
			wantHosts:  []string{githubAPIConnectHost, fallbackConnectHost},
		},
		{
			name:       "other repository id API URL skips fallback",
			repository: "https://api.github.com/repositories/12345/releases/latest",
			wantHosts:  []string{githubAPIConnectHost},
		},
		{
			name:       "unrecognized repository uses fork default and skips fallback",
			repository: "https://gitlab.com/router-for-me/Cli-Proxy-API-Management-Center",
			wantHosts:  []string{githubAPIConnectHost},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captureWarnings(t)
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

// loggedMessages returns the warning and error messages captured by hook, in order.
func loggedMessages(hook *logtest.Hook) []string {
	var messages []string
	for _, entry := range hook.AllEntries() {
		if entry.Level <= log.WarnLevel {
			messages = append(messages, entry.Message)
		}
	}
	return messages
}

func respond(status int, body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func TestLogSafeURL(t *testing.T) {
	tests := map[string]string{
		"https://user:secret@api.github.com/repos/o/r?token=abc#frag": "https://api.github.com/repos/o/r",
		"github.com/o/r?token=abc":                                    "github.com/o/r",
		"user:secret@gitlab.com/o/r":                                  "gitlab.com/o/r",
		"token@gitlab.com/o/r":                                        "gitlab.com/o/r",
		"git@github.com:o/r.git":                                      "github.com:o/r.git",
		"://not a url":                                                "://not a url",
		"https://gitlab.com/@group/proj":                              "https://gitlab.com/@group/proj",
		// A "?" or "#" before the last "@" is ambiguous: credentials or a secret query could leak either way.
		"https://user:p#ss@github.com/o/r":                                 redactedLogValue,
		"https://user:p?ss@github.com/o/r":                                 redactedLogValue,
		"user:p#ss@x/y":                                                    redactedLogValue,
		"user:p?ss@x/y":                                                    redactedLogValue,
		"https://u:1234/x#y@h/o/r":                                         redactedLogValue,
		"https://gitlab.com/o/r?x=a@b&token=QSECRET":                       redactedLogValue,
		"https://gitlab.com/o/r?email=me@example.com&private_token=Q":      redactedLogValue,
		"https://gitlab.com/o/r#frag@QSECRET":                              redactedLogValue,
		"https://gitlab.com/o/r?a=b@c/QSECRET":                             redactedLogValue,
		"gitlab.com/o/r?x=a@b&token=QSECRET":                               redactedLogValue,
		"https://api.github.com/repositories/1/releases?login=me@x.io&t=Q": redactedLogValue,
	}
	for raw, want := range tests {
		got := logSafeURL(raw)
		if got != want {
			t.Errorf("logSafeURL(%q) = %q, want %q", raw, got, want)
		}
		for _, secret := range []string{"secret", "QSECRET", "p#ss", "p?ss", "1234", "abc"} {
			if strings.Contains(raw, secret) && strings.Contains(got, secret) {
				t.Errorf("logSafeURL(%q) = %q leaks %q", raw, got, secret)
			}
		}
	}
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
