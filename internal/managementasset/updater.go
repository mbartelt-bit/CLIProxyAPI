package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/githubauth"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpfetch"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	defaultManagementReleaseURL  = "https://api.github.com/repos/mbartelt-bit/Cli-Proxy-API-Management-Center/releases/latest"
	upstreamPanelOwner           = "router-for-me"
	upstreamPanelName            = "Cli-Proxy-API-Management-Center"
	upstreamPanelRepositoryURL   = "https://github.com/" + upstreamPanelOwner + "/" + upstreamPanelName
	upstreamManagementReleaseURL = "https://api.github.com/repos/" + upstreamPanelOwner + "/" + upstreamPanelName + "/releases/latest"
	defaultManagementFallbackURL = "https://cpamc.router-for.me/" // Hosted build of the upstream panel only.
	managementAssetName          = "management.html"
	httpUserAgent                = "CLIProxyAPI-management-updater"
	managementSyncMinInterval    = 30 * time.Second
	updateCheckInterval          = 3 * time.Hour
	maxAssetDownloadSize         = 50 << 20 // 10 MB safety limit for management asset downloads
)

// upstreamPanelRepositoryID is GitHub's numeric id of the upstream panel repository, used by
// api.github.com/repositories/{id} URLs (GitHub also redirects renamed or transferred repositories to that form).
// Read from `gh api repos/router-for-me/Cli-Proxy-API-Management-Center --jq .id`; the id survives renames.
const upstreamPanelRepositoryID = "1051566067"

// redactedLogValue replaces a configured URL that cannot be logged without risking credentials or secrets.
const redactedLogValue = "<redacted: value may contain credentials>"

// ManagementFileName exposes the control panel asset filename.
const ManagementFileName = managementAssetName

// errManagementAssetNotInRelease reports a latest release that carries no management.html asset.
var errManagementAssetNotInRelease = errors.New("management asset " + managementAssetName + " not found in latest release")

var (
	// githubOwnerPattern and githubNamePattern accept the characters GitHub allows in owner and repository
	// names. Owners never contain dots, which tells a bare "owner/repo" apart from "github.com/owner".
	githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	githubNamePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// githubRepositoryIDPattern accepts a numeric repository id in canonical form, so ids compare as strings.
	githubRepositoryIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
	// scpLikeGitPattern matches SSH remotes written as user@host:owner/repo.git.
	scpLikeGitPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+@([A-Za-z0-9.-]+):(.+)$`)
)

var (
	lastUpdateCheckMu   sync.Mutex
	lastUpdateCheckTime time.Time
	currentConfigPtr    atomic.Pointer[config.Config]
	schedulerOnce       sync.Once
	schedulerConfigPath atomic.Value
	sfGroup             singleflight.Group
)

// SetCurrentConfig stores the latest configuration snapshot for management asset decisions.
func SetCurrentConfig(cfg *config.Config) {
	if cfg == nil {
		currentConfigPtr.Store(nil)
		return
	}
	currentConfigPtr.Store(cfg)
}

// StartAutoUpdater launches a background goroutine that periodically ensures the management asset is up to date.
// It respects the disable-control-panel flag on every iteration and supports hot-reloaded configurations.
func StartAutoUpdater(ctx context.Context, configFilePath string) {
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		log.Debug("management asset auto-updater skipped: empty config path")
		return
	}

	schedulerConfigPath.Store(configFilePath)

	schedulerOnce.Do(func() {
		go runAutoUpdater(ctx)
	})
}

func runAutoUpdater(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()

	runOnce := func() {
		cfg := currentConfigPtr.Load()
		if reason, skip := autoUpdateSkipReason(cfg); skip {
			log.Debugf("management asset auto-updater skipped: %s", reason)
			return
		}

		configPath, _ := schedulerConfigPath.Load().(string)
		staticDir := StaticDir(configPath)
		EnsureLatestManagementHTML(ctx, staticDir, cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository)
	}

	runOnce()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func autoUpdateSkipReason(cfg *config.Config) (string, bool) {
	if cfg == nil {
		return "config not yet available", true
	}
	if cfg.Home.Enabled {
		return "cluster mode enabled", true
	}
	if cfg.RemoteManagement.DisableControlPanel {
		return "control panel disabled", true
	}
	if cfg.RemoteManagement.DisableAutoUpdatePanel {
		return "disable-auto-update-panel is enabled", true
	}
	return "", false
}

func newHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{Timeout: 15 * time.Second}

	sdkCfg := &sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
	util.SetProxy(sdkCfg, client)

	return client
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type releaseResponse struct {
	Assets []releaseAsset `json:"assets"`
}

// StaticDir resolves the directory that stores the management control panel asset.
func StaticDir(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return filepath.Dir(cleaned)
		}
		return cleaned
	}

	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "static")
	}

	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}

	base := filepath.Dir(configFilePath)
	fileInfo, err := os.Stat(configFilePath)
	if err == nil {
		if fileInfo.IsDir() {
			base = configFilePath
		}
	}

	return filepath.Join(base, "static")
}

// FilePath resolves the absolute path to the management control panel asset.
func FilePath(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return cleaned
		}
		return filepath.Join(cleaned, ManagementFileName)
	}

	dir := StaticDir(configFilePath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ManagementFileName)
}

// EnsureLatestManagementHTML checks the latest management.html asset and updates the local copy when needed.
// It coalesces concurrent sync attempts and returns whether the asset exists after the sync attempt.
func EnsureLatestManagementHTML(ctx context.Context, staticDir string, proxyURL string, panelRepository string) bool {
	if ctx == nil {
		ctx = context.Background()
	}

	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		log.Debug("management asset sync skipped: empty static directory")
		return false
	}
	localPath := filepath.Join(staticDir, managementAssetName)

	_, _, _ = sfGroup.Do(localPath, func() (interface{}, error) {
		lastUpdateCheckMu.Lock()
		now := time.Now()
		timeSinceLastAttempt := now.Sub(lastUpdateCheckTime)
		if !lastUpdateCheckTime.IsZero() && timeSinceLastAttempt < managementSyncMinInterval {
			lastUpdateCheckMu.Unlock()
			log.Debugf(
				"management asset sync skipped by throttle: last attempt %v ago (interval %v)",
				timeSinceLastAttempt.Round(time.Second),
				managementSyncMinInterval,
			)
			return nil, nil
		}
		lastUpdateCheckTime = now
		lastUpdateCheckMu.Unlock()

		localFileMissing := false
		if _, errStat := os.Stat(localPath); errStat != nil {
			if errors.Is(errStat, os.ErrNotExist) {
				localFileMissing = true
			} else {
				log.WithError(errStat).Debug("failed to stat local management asset")
			}
		}

		if errMkdirAll := os.MkdirAll(staticDir, 0o755); errMkdirAll != nil {
			log.WithError(errMkdirAll).Warn("failed to prepare static directory for management asset")
			return nil, nil
		}

		syncManagementAsset(ctx, newHTTPClient(proxyURL), localPath, resolveReleaseURL(panelRepository), localFileMissing)
		return nil, nil
	})

	_, err := os.Stat(localPath)
	return err == nil
}

// syncManagementAsset downloads the management.html asset of the latest release at releaseURL into localPath
// when it differs from the local copy.
func syncManagementAsset(ctx context.Context, client *http.Client, localPath string, releaseURL string, localFileMissing bool) {
	localHash, err := fileSHA256(localPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.WithError(err).Debug("failed to read local management asset hash")
		}
		localHash = ""
	}

	asset, remoteHash, err := fetchLatestAsset(ctx, client, releaseURL)
	if err != nil {
		handleManagementSyncFailure(ctx, client, localPath, releaseURL, localFileMissing, syncStageReleaseLookup, err)
		return
	}

	if remoteHash != "" && localHash != "" && strings.EqualFold(remoteHash, localHash) {
		log.Debug("management asset is already up to date")
		return
	}

	data, downloadedHash, err := downloadAsset(ctx, client, asset.BrowserDownloadURL)
	if err != nil {
		handleManagementSyncFailure(ctx, client, localPath, releaseURL, localFileMissing, syncStageAssetDownload, err)
		return
	}

	// A digest mismatch never falls back to the hosted page, for any repository: the release itself is suspect.
	if remoteHash != "" && !strings.EqualFold(remoteHash, downloadedHash) {
		log.Errorf("management asset digest mismatch: expected %s got %s — aborting update for safety", remoteHash, downloadedHash)
		return
	}

	if err = atomicWriteFile(localPath, data); err != nil {
		log.WithError(err).Warn("failed to update management asset on disk")
		return
	}

	log.Infof("management asset updated successfully (hash=%s)", downloadedHash)
}

// syncStage names the step of a management asset sync that failed.
type syncStage int

const (
	syncStageReleaseLookup syncStage = iota
	syncStageAssetDownload
)

func (s syncStage) failureMessage() string {
	if s == syncStageAssetDownload {
		return "failed to download management asset"
	}
	return "failed to fetch latest management release information"
}

// handleManagementSyncFailure logs a failed release lookup or asset download. When no local copy exists yet
// it installs the hosted fallback page, but only for the upstream panel repository: that page is the upstream
// panel build, so any other repository is left without a panel and the single warning says what to fix.
func handleManagementSyncFailure(ctx context.Context, client *http.Client, localPath string, releaseURL string, localFileMissing bool, stage syncStage, err error) {
	entry := log.WithError(err)
	switch {
	case !localFileMissing:
		entry.Warn(stage.failureMessage())
	case isUpstreamManagementReleaseURL(releaseURL):
		entry.Warn(stage.failureMessage() + ", trying fallback page")
		ensureFallbackManagementHTML(ctx, client, localPath)
	default:
		repository := releaseRepositoryLabel(releaseURL)
		entry.Warnf("%s; no management panel is available until a sync succeeds (the fallback page %s is the upstream panel build, so it is not used for %s); %s",
			stage.failureMessage(), defaultManagementFallbackURL, repository, nonUpstreamSyncFailureHint(stage, err, repository))
	}
}

// nonUpstreamSyncFailureHint tells the operator how to recover from a failed sync of a panel repository that has
// no fallback page. Only a release lookup that found no release gets the "publish a release" remedy.
func nonUpstreamSyncFailureHint(stage syncStage, err error, repository string) string {
	switch {
	case stage == syncStageAssetDownload:
		return "the latest release lists the asset, so check network access and proxy-url; the download is retried on the next sync"
	case managementReleaseMissing(err):
		return fmt.Sprintf("GitHub reports no published release with %s for %s (a missing or private repository also returns 404). "+
			"Publish one: on a fork, first enable GitHub Actions at %s (GitHub disables workflows on new forks), "+
			"then push a vX.Y.Z tag that does not already exist there so its release workflow runs. Or set management.panel-github-repository to %s",
			managementAssetName, repository, githubActionsPage(repository), upstreamPanelRepositoryURL)
	default:
		return "the release lookup is retried on the next sync"
	}
}

// managementReleaseMissing reports whether a release lookup failed because no published release carries
// management.html. GitHub answers 404 when the repository has no non-draft, non-prerelease release, and also
// when the repository does not exist or is private.
func managementReleaseMissing(err error) bool {
	var statusErr *httpfetch.StatusError
	return errors.Is(err, errManagementAssetNotInRelease) ||
		(errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound)
}

// releaseRepositoryLabel names the repository behind a release URL for log messages: owner/name, or its
// numeric id when the release URL uses the api.github.com/repositories/{id} form.
func releaseRepositoryLabel(releaseURL string) string {
	repository, ok := parsePanelRepository(releaseURL)
	switch {
	case !ok:
		return logSafeURL(releaseURL)
	case repository.id != "":
		return "repository id " + repository.id
	default:
		return repository.owner + "/" + repository.name
	}
}

// githubActionsPage names where GitHub Actions is enabled for a repository labelled by releaseRepositoryLabel.
// Only an owner/name label yields a page URL.
func githubActionsPage(repository string) string {
	if owner, name, ok := strings.Cut(repository, "/"); ok && githubOwnerPattern.MatchString(owner) && githubNamePattern.MatchString(name) {
		return "https://github.com/" + repository + "/actions"
	}
	return "the Actions tab of " + repository
}

// ensureFallbackManagementHTML downloads the hosted fallback page into localPath. The page is the upstream
// panel build, so callers must only use it for the upstream panel repository.
func ensureFallbackManagementHTML(ctx context.Context, client *http.Client, localPath string) bool {
	data, downloadedHash, err := downloadAsset(ctx, client, defaultManagementFallbackURL)
	if err != nil {
		log.WithError(err).Warn("failed to download fallback management control panel page")
		return false
	}

	log.Warnf("management asset downloaded from fallback URL without digest verification (hash=%s) — "+
		"enable verified GitHub updates by keeping disable-auto-update-panel set to false", downloadedHash)

	if err = atomicWriteFile(localPath, data); err != nil {
		log.WithError(err).Warn("failed to persist fallback management control panel page")
		return false
	}

	log.Infof("management asset updated from fallback page successfully (hash=%s)", downloadedHash)
	return true
}

// panelRepository identifies the GitHub repository that publishes the management panel.
type panelRepository struct {
	owner string
	name  string
	// id is set instead of owner and name when the repository is named by its numeric GitHub id, as in
	// api.github.com/repositories/{id}.
	id string
	// user keeps credentials written into an api.github.com URL, which the release request has always sent.
	// Credentials written for github.com are never forwarded to the API host.
	user *url.Userinfo
}

// releaseURL returns the GitHub API URL of the repository's latest release.
func (r panelRepository) releaseURL() string {
	path := "/repos/" + r.owner + "/" + r.name + "/releases/latest"
	if r.id != "" {
		path = "/repositories/" + r.id + "/releases/latest"
	}
	return (&url.URL{Scheme: "https", User: r.user, Host: "api.github.com", Path: path}).String()
}

// isUpstream reports whether r is the upstream panel repository. GitHub owner and repository names are
// case-insensitive.
func (r panelRepository) isUpstream() bool {
	if r.id != "" {
		return r.id == upstreamPanelRepositoryID
	}
	return strings.EqualFold(r.owner, upstreamPanelOwner) && strings.EqualFold(r.name, upstreamPanelName)
}

// parsePanelRepository extracts the GitHub owner and repository name from a configured panel repository.
// It accepts github.com and www.github.com repository URLs with or without a scheme (extra path segments such
// as /releases are ignored), api.github.com /repos/{owner}/{repo} and /repositories/{id} URLs with or without
// /releases or /releases/latest, SSH remotes (ssh://git@github.com/... and git@github.com:owner/repo), and a bare
// owner/repo. Hosts are case-insensitive; ports, query, fragment, trailing slashes and a ".git" suffix are
// ignored.
func parsePanelRepository(raw string) (panelRepository, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return panelRepository{}, false
	}

	hasScheme := strings.Contains(raw, "://")
	bareParts := strings.Split(strings.Trim(raw, "/"), "/")
	var host, path string
	var user *url.Userinfo
	switch {
	case !hasScheme && scpLikeGitPattern.MatchString(raw):
		match := scpLikeGitPattern.FindStringSubmatch(raw)
		host, path = match[1], match[2]
	case !hasScheme && len(bareParts) == 2 && !strings.Contains(bareParts[0], "."):
		host, path = "github.com", raw
	default:
		if !hasScheme {
			raw = "https://" + raw
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return panelRepository{}, false
		}
		host, path, user = parsed.Hostname(), parsed.Path, parsed.User
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")
	var repository panelRepository
	switch strings.ToLower(host) {
	case "github.com", "www.github.com":
		if len(segments) < 2 {
			return panelRepository{}, false
		}
		repository = panelRepository{owner: segments[0], name: segments[1]}
	case "api.github.com":
		if len(segments) >= 2 && strings.EqualFold(segments[0], "repositories") {
			// GitHub resolves ids with leading zeros, so normalize before comparing.
			if id := strings.TrimLeft(segments[1], "0"); githubRepositoryIDPattern.MatchString(id) {
				return panelRepository{id: id, user: user}, true
			}
		}
		if len(segments) < 3 || !strings.EqualFold(segments[0], "repos") {
			return panelRepository{}, false
		}
		repository = panelRepository{owner: segments[1], name: segments[2], user: user}
	default:
		return panelRepository{}, false
	}

	if len(repository.name) > len(".git") && strings.EqualFold(repository.name[len(repository.name)-len(".git"):], ".git") {
		repository.name = repository.name[:len(repository.name)-len(".git")]
	}
	if !githubOwnerPattern.MatchString(repository.owner) || !githubNamePattern.MatchString(repository.name) ||
		repository.name == "." || repository.name == ".." {
		return panelRepository{}, false
	}
	return repository, true
}

// resolveReleaseURL maps the configured panel repository to its GitHub latest-release API URL. An empty value
// selects the default panel repository. A value that names no GitHub repository also falls back to the
// default, with a warning so the ignored setting is visible.
func resolveReleaseURL(repo string) string {
	if strings.TrimSpace(repo) == "" {
		return defaultManagementReleaseURL
	}
	if repository, ok := parsePanelRepository(repo); ok {
		return repository.releaseURL()
	}
	log.Warnf("management.panel-github-repository %q is not a recognized GitHub repository; using the default panel repository %s",
		logSafeURL(repo), config.DefaultPanelGitHubRepository)
	return defaultManagementReleaseURL
}

// isUpstreamManagementReleaseURL reports whether a release URL belongs to the upstream panel repository. It
// compares the GitHub owner and repository name (or the numeric repository id), so scheme, port, credentials,
// query and the releases suffix do not matter.
func isUpstreamManagementReleaseURL(releaseURL string) bool {
	repository, ok := parsePanelRepository(releaseURL)
	return ok && repository.isUpstream()
}

// logSafeURL drops credentials, query and fragment so a configured URL can be logged. When a "?" or "#"
// appears before the last "@", the value cannot be split safely: the "@" may end unencoded credentials, or
// it may sit inside a query or fragment whose remainder is secret. Such values are replaced by a fixed
// placeholder rather than guessed at.
func logSafeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		if cut := strings.IndexAny(raw, "?#"); cut >= 0 && cut < at {
			return redactedLogValue
		}
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}).String()
	}
	// Scheme-less and scp-style values: everything up to the last "@" (keeping any scheme) is userinfo.
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		scheme := ""
		if j := strings.Index(raw, "://"); j >= 0 && j < at {
			scheme = raw[:j+len("://")]
		}
		raw = scheme + raw[at+1:]
	}
	if cut := strings.IndexAny(raw, "?#"); cut >= 0 {
		raw = raw[:cut]
	}
	return raw
}

func fetchLatestAsset(ctx context.Context, client *http.Client, releaseURL string) (*releaseAsset, string, error) {
	if strings.TrimSpace(releaseURL) == "" {
		releaseURL = defaultManagementReleaseURL
	}

	headers := map[string]string{
		"Accept":     "application/vnd.github+json",
		"User-Agent": httpUserAgent,
	}
	if token := githubauth.TokenForURL(releaseURL); token != "" {
		headers["Authorization"] = "Bearer " + token
	}

	data, err := httpfetch.GetBytes(ctx, client, releaseURL, headers, 0)
	if err != nil {
		return nil, "", fmt.Errorf("fetch release: %w", err)
	}

	var release releaseResponse
	if err = json.Unmarshal(data, &release); err != nil {
		return nil, "", fmt.Errorf("decode release response: %w", err)
	}

	for i := range release.Assets {
		asset := &release.Assets[i]
		if strings.EqualFold(asset.Name, managementAssetName) {
			remoteHash := parseDigest(asset.Digest)
			return asset, remoteHash, nil
		}
	}

	return nil, "", errManagementAssetNotInRelease
}

func downloadAsset(ctx context.Context, client *http.Client, downloadURL string) ([]byte, string, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, "", fmt.Errorf("empty download url")
	}

	data, err := httpfetch.GetBytes(ctx, client, downloadURL, map[string]string{"User-Agent": httpUserAgent}, maxAssetDownloadSize)
	if err != nil {
		return nil, "", fmt.Errorf("download asset: %w", err)
	}

	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
	}()

	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func atomicWriteFile(path string, data []byte) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), "management-*.html")
	if err != nil {
		return err
	}

	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err = tmpFile.Write(data); err != nil {
		return err
	}

	if err = tmpFile.Chmod(0o644); err != nil {
		return err
	}

	if err = tmpFile.Close(); err != nil {
		return err
	}

	if err = os.Rename(tmpName, path); err != nil {
		return err
	}

	return nil
}

func parseDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return ""
	}

	if idx := strings.Index(digest, ":"); idx >= 0 {
		digest = digest[idx+1:]
	}

	return strings.ToLower(strings.TrimSpace(digest))
}
