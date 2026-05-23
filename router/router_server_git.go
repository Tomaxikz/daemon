package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment/docker"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const gitContainerRoot = "/home/container"
const gitBetterFilesTrashDir = ".trash-bin"

type gitCloneTargetMode int

const (
	gitCloneTargetEmpty gitCloneTargetMode = iota
	gitCloneTargetInternalOnly
)

var (
	gitPathCache sync.Map
	gitLocks     sync.Map

	gitTrustedBinaries = []string{
		"/usr/bin/git",
		"/usr/local/bin/git",
		"/bin/git",
	}

	gitSafeEnv = []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_ALLOW_PROTOCOL=https",
		"HOME=/tmp",
	}

	gitBlockedNetworks = []*net.IPNet{
		mustParseGitCIDR("0.0.0.0/8"),
		mustParseGitCIDR("10.0.0.0/8"),
		mustParseGitCIDR("100.64.0.0/10"),
		mustParseGitCIDR("127.0.0.0/8"),
		mustParseGitCIDR("169.254.0.0/16"),
		mustParseGitCIDR("172.16.0.0/12"),
		mustParseGitCIDR("192.0.0.0/24"),
		mustParseGitCIDR("192.0.2.0/24"),
		mustParseGitCIDR("192.168.0.0/16"),
		mustParseGitCIDR("198.18.0.0/15"),
		mustParseGitCIDR("198.51.100.0/24"),
		mustParseGitCIDR("203.0.113.0/24"),
		mustParseGitCIDR("224.0.0.0/4"),
		mustParseGitCIDR("240.0.0.0/4"),
		mustParseGitCIDR("255.255.255.255/32"),
		mustParseGitCIDR("::/128"),
		mustParseGitCIDR("::1/128"),
		mustParseGitCIDR("2001:db8::/32"),
		mustParseGitCIDR("fc00::/7"),
		mustParseGitCIDR("fe80::/10"),
		mustParseGitCIDR("fec0::/10"),
		mustParseGitCIDR("ff00::/8"),
	}

	gitTargetDirectoryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	gitRemoteNamePattern      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	gitCredentialPattern      = regexp.MustCompile(`(?i)(https://)([^/@\s]+)@`)
)

type gitCloneRequest struct {
	RepositoryURL   string `json:"repository_url" binding:"required"`
	TargetDirectory string `json:"target_directory"`
	WorkDir         string `json:"work_dir"`
}

type gitPullRequest struct {
	WorkDir string `json:"work_dir"`
}

type gitDiffRequest struct {
	WorkDir string `json:"work_dir"`
	File    string `json:"file"`
}

type gitResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type validatedGitRepositoryURL struct {
	URL            string
	CurlOptResolve string
}

func findGitBinary(ctx context.Context, env *docker.Environment) string {
	if cached, ok := gitPathCache.Load(env.Id); ok {
		return cached.(string)
	}

	for _, p := range gitTrustedBinaries {
		res, err := execInContainer(ctx, env, []string{p, "--version"}, gitContainerRoot)
		if err == nil && res.ExitCode == 0 {
			gitPathCache.Store(env.Id, p)
			return p
		}
	}

	return ""
}

func getServerGitStatus(c *gin.Context) {
	s := middleware.ExtractServer(c)

	env, ok := s.Environment.(*docker.Environment)
	if !ok {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git operations are only supported in Docker environments.",
		})
		return
	}

	workDir, err := normalizeGitWorkDir(c.Query("work_dir"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	gitBin := findGitBinary(ctx, env)
	if gitBin == "" {
		c.JSON(http.StatusOK, gin.H{
			"available": false,
			"is_repo":   false,
		})
		return
	}

	checkResult, err := execGit(ctx, env, gitBin, []string{"rev-parse", "--is-inside-work-tree"}, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if checkResult.ExitCode != 0 {
		c.JSON(http.StatusOK, gin.H{
			"available": true,
			"is_repo":   false,
			"error":     strings.TrimSpace(maskGitOutput(checkResult.Stderr + checkResult.Stdout)),
		})
		return
	}

	statusResult, _ := execGit(ctx, env, gitBin, []string{"status", "--porcelain", "-b"}, workDir)
	branchResult, _ := execGit(ctx, env, gitBin, []string{"rev-parse", "--abbrev-ref", "HEAD"}, workDir)
	remoteResult, _ := execGit(ctx, env, gitBin, []string{"remote", "-v"}, workDir)
	logResult, _ := execGit(ctx, env, gitBin, []string{"log", "--oneline", "-20"}, workDir)

	c.JSON(http.StatusOK, gin.H{
		"available": true,
		"is_repo":   true,
		"branch":    strings.TrimSpace(branchResult.Stdout),
		"status":    statusResult.Stdout,
		"remotes":   maskGitOutput(remoteResult.Stdout),
		"log":       logResult.Stdout,
	})
}

func postServerGitClone(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var req gitCloneRequest
	if err := c.BindJSON(&req); err != nil {
		return
	}

	env, ok := s.Environment.(*docker.Environment)
	if !ok {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git operations are only supported in Docker environments.",
		})
		return
	}

	workDir, err := normalizeGitWorkDir(req.WorkDir)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()

	repositoryURL, err := validateGitRepositoryURL(ctx, req.RepositoryURL)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	targetDirectory := strings.TrimSpace(req.TargetDirectory)
	targetPath := workDir
	if targetDirectory != "" {
		if err := validateGitTargetDirectory(targetDirectory); err != nil {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		targetPath = path.Join(workDir, targetDirectory)
	}

	if !isInsideGitRoot(targetPath) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Target directory must stay inside the server files."})
		return
	}

	targetServerPath := containerPathToServerPath(targetPath)

	gitBin := findGitBinary(ctx, env)
	if gitBin == "" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git is not installed in this container image.",
		})
		return
	}

	lock := gitLockFor(env.Id)
	lock.Lock()
	defer lock.Unlock()

	targetMode, err := inspectGitCloneTarget(s.Filesystem(), targetServerPath)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	cloneTargetPath := targetPath
	var tempServerPath string
	if targetMode == gitCloneTargetInternalOnly {
		var err error
		cloneTargetPath, tempServerPath, err = makeGitCloneTempTarget(s.Filesystem(), targetPath)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		defer func() {
			_ = s.Filesystem().Delete(tempServerPath)
		}()
	}

	result, err := execGit(ctx, env, gitBin, []string{
		"clone",
		"--depth=1",
		"--single-branch",
		"--no-tags",
		"--no-recurse-submodules",
		repositoryURL.URL,
		cloneTargetPath,
	}, workDir, repositoryURL)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if result.ExitCode == 0 {
		if targetMode == gitCloneTargetInternalOnly {
			if err := promoteGitCloneTempTarget(s.Filesystem(), tempServerPath, targetServerPath); err != nil {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
				return
			}
		}
		_ = addGitInternalExcludes(s.Filesystem(), targetServerPath)
	}
	if cloneTargetPath != targetPath {
		result.Stdout = strings.ReplaceAll(result.Stdout, cloneTargetPath, targetPath)
		result.Stderr = strings.ReplaceAll(result.Stderr, cloneTargetPath, targetPath)
	}

	c.JSON(http.StatusOK, maskGitResponse(result))
}

func postServerGitPull(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var req gitPullRequest
	if err := c.BindJSON(&req); err != nil {
		return
	}

	env, ok := s.Environment.(*docker.Environment)
	if !ok {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git operations are only supported in Docker environments.",
		})
		return
	}

	workDir, err := normalizeGitWorkDir(req.WorkDir)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Minute)
	defer cancel()

	gitBin := findGitBinary(ctx, env)
	if gitBin == "" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git is not installed in this container image.",
		})
		return
	}

	checkResult, err := execGit(ctx, env, gitBin, []string{"rev-parse", "--is-inside-work-tree"}, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if checkResult.ExitCode != 0 {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "This directory is not a Git repository."})
		return
	}

	if err := ensureNoDangerousLocalGitConfig(ctx, env, gitBin, workDir); err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	branchResult, err := execGit(ctx, env, gitBin, []string{"rev-parse", "--abbrev-ref", "HEAD"}, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	branch := strings.TrimSpace(branchResult.Stdout)
	if branch == "" || branch == "HEAD" || !isSafeGitRef(branch) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "Cannot pull while the repository is detached or has an invalid branch."})
		return
	}

	remoteNameResult, err := execGit(ctx, env, gitBin, []string{"config", "--get", "branch." + branch + ".remote"}, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	remoteName := strings.TrimSpace(remoteNameResult.Stdout)
	if remoteName == "" {
		remoteName = "origin"
	}
	if !gitRemoteNamePattern.MatchString(remoteName) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Repository remote name is not allowed."})
		return
	}

	mergeRefResult, _ := execGit(ctx, env, gitBin, []string{"config", "--get", "branch." + branch + ".merge"}, workDir)
	remoteBranch := strings.TrimSpace(mergeRefResult.Stdout)
	remoteBranch = strings.TrimPrefix(remoteBranch, "refs/heads/")
	if remoteBranch == "" {
		remoteBranch = branch
	}
	if !isSafeGitRef(remoteBranch) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Repository upstream branch is not allowed."})
		return
	}

	remoteURLResult, err := execGit(ctx, env, gitBin, []string{"remote", "get-url", remoteName}, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	remoteURL, err := validateGitRepositoryURL(ctx, remoteURLResult.Stdout)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Repository remote is not allowed: " + err.Error()})
		return
	}

	lock := gitLockFor(env.Id)
	lock.Lock()
	defer lock.Unlock()

	result, err := execGit(ctx, env, gitBin, []string{
		"pull",
		"--ff-only",
		"--no-recurse-submodules",
		remoteURL.URL,
		remoteBranch,
	}, workDir, remoteURL)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, maskGitResponse(result))
}

func postServerGitDiff(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var req gitDiffRequest
	if err := c.BindJSON(&req); err != nil {
		return
	}

	env, ok := s.Environment.(*docker.Environment)
	if !ok {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git operations are only supported in Docker environments.",
		})
		return
	}

	workDir, err := normalizeGitWorkDir(req.WorkDir)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	diffPath, err := normalizeGitDiffPath(req.File)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	gitBin := findGitBinary(ctx, env)
	if gitBin == "" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git is not installed in this container image.",
		})
		return
	}

	checkResult, err := execGit(ctx, env, gitBin, []string{"rev-parse", "--is-inside-work-tree"}, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if checkResult.ExitCode != 0 {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "This directory is not a Git repository."})
		return
	}

	if err := ensureNoDangerousLocalGitConfig(ctx, env, gitBin, workDir); err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	cachedArgs := gitDiffArgs(true, diffPath)
	unstagedArgs := gitDiffArgs(false, diffPath)

	cachedResult, err := execGit(ctx, env, gitBin, cachedArgs, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	unstagedResult, err := execGit(ctx, env, gitBin, unstagedArgs, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	result := &gitResponse{
		Stdout:   combineGitDiffOutput(cachedResult.Stdout, unstagedResult.Stdout),
		Stderr:   strings.TrimSpace(cachedResult.Stderr + "\n" + unstagedResult.Stderr),
		ExitCode: 0,
	}
	if cachedResult.ExitCode != 0 {
		result.ExitCode = cachedResult.ExitCode
	} else if unstagedResult.ExitCode != 0 {
		result.ExitCode = unstagedResult.ExitCode
	}
	if result.Stdout == "" && result.Stderr == "" {
		result.Stdout = "(no tracked diff)"
	}

	c.JSON(http.StatusOK, maskGitResponse(result))
}

func execGit(ctx context.Context, env *docker.Environment, gitBin string, args []string, workDir string, remotes ...validatedGitRepositoryURL) (*gitResponse, error) {
	cmd := gitBaseCommand(gitBin)
	for _, remote := range remotes {
		if remote.CurlOptResolve == "" {
			continue
		}
		cmd = append(cmd, "-c", "http.curloptResolve="+remote.CurlOptResolve)
	}
	cmd = append(cmd, args...)
	return execInContainer(ctx, env, cmd, workDir)
}

func gitBaseCommand(gitBin string) []string {
	return []string{
		gitBin,
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.git.allow=never",
		"-c", "protocol.ssh.allow=never",
		"-c", "credential.helper=",
		"-c", "core.askPass=",
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "submodule.recurse=false",
		"-c", "diff.external=",
		"-c", "advice.detachedHead=false",
	}
}

func ensureNoDangerousLocalGitConfig(ctx context.Context, env *docker.Environment, gitBin string, workDir string) error {
	result, err := execGit(ctx, env, gitBin, []string{
		"config",
		"--local",
		"--get-regexp",
		`^(filter\..*\.(process|smudge|clean)|merge\..*\.driver|diff\..*\.(command|textconv)|core\.sshCommand|core\.fsmonitor|credential\.helper|url\..*\.(insteadOf|pushInsteadOf)|include.*)$`,
	}, workDir)
	if err != nil {
		return err
	}

	if result.ExitCode == 0 && strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("Repository local Git config contains unsafe helper settings.")
	}

	return nil
}

func normalizeGitWorkDir(raw string) (string, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" || cleaned == "/" {
		return gitContainerRoot, nil
	}
	if strings.ContainsAny(cleaned, "\x00\n\r\\") {
		return "", fmt.Errorf("Invalid work directory.")
	}

	var normalized string
	if strings.HasPrefix(cleaned, gitContainerRoot) {
		normalized = path.Clean(cleaned)
	} else {
		normalized = path.Join(gitContainerRoot, strings.TrimPrefix(cleaned, "/"))
	}

	if !isInsideGitRoot(normalized) {
		return "", fmt.Errorf("Work directory must stay inside the server files.")
	}

	return normalized, nil
}

func normalizeGitDiffPath(raw string) (string, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return "", nil
	}
	if len(cleaned) > 4096 || strings.ContainsAny(cleaned, "\x00\n\r\\") {
		return "", fmt.Errorf("Diff path is invalid.")
	}
	if strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "-") || strings.HasPrefix(cleaned, ":") {
		return "", fmt.Errorf("Diff path is not allowed.")
	}

	normalized := path.Clean(cleaned)
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || strings.Contains(normalized, "/../") {
		return "", fmt.Errorf("Diff path must stay inside the repository.")
	}

	return normalized, nil
}

func isInsideGitRoot(p string) bool {
	cleaned := path.Clean(p)
	return cleaned == gitContainerRoot || strings.HasPrefix(cleaned, gitContainerRoot+"/")
}

func validateGitRepositoryURL(ctx context.Context, raw string) (validatedGitRepositoryURL, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.Trim(cleaned, "\x00\r\n\t ")
	if strings.ContainsAny(cleaned, "\x00\n\r\t") {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository URL is invalid.")
	}
	if cleaned == "" || len(cleaned) > 2048 {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository URL is invalid.")
	}

	parsed, err := url.Parse(cleaned)
	if err != nil {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository URL is invalid.")
	}
	if parsed.Scheme != "https" {
		return validatedGitRepositoryURL{}, fmt.Errorf("Only HTTPS Git remotes are allowed.")
	}
	if parsed.User != nil {
		return validatedGitRepositoryURL{}, fmt.Errorf("Credentials in Git remote URLs are not allowed.")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository URL must not include query strings or fragments.")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository URL is missing a repository path.")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository host is not allowed.")
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedGitIP(ip) {
			return validatedGitRepositoryURL{}, fmt.Errorf("Repository host resolves to an internal network.")
		}
		return validatedGitRepositoryURL{URL: parsed.String()}, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil || len(addrs) == 0 {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository host could not be resolved.")
	}
	var resolved net.IP
	for _, addr := range addrs {
		if isBlockedGitIP(addr.IP) {
			return validatedGitRepositoryURL{}, fmt.Errorf("Repository host resolves to an internal network.")
		}
		if resolved == nil {
			resolved = addr.IP
		}
	}
	if resolved == nil {
		return validatedGitRepositoryURL{}, fmt.Errorf("Repository host could not be resolved.")
	}

	return validatedGitRepositoryURL{
		URL:            parsed.String(),
		CurlOptResolve: gitCurlOptResolve(host, parsed.Port(), resolved),
	}, nil
}

func gitCurlOptResolve(host string, port string, ip net.IP) string {
	if port == "" {
		port = "443"
	}
	address := ip.String()
	if strings.Contains(address, ":") {
		address = "[" + address + "]"
	}
	return host + ":" + port + ":" + address
}

func validateGitTargetDirectory(name string) error {
	if name == "." || name == ".." || strings.Contains(name, "..") || strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("Target directory name is not allowed.")
	}
	if !gitTargetDirectoryPattern.MatchString(name) {
		return fmt.Errorf("Target directory can only contain letters, numbers, dots, underscores, and dashes.")
	}
	return nil
}

func inspectGitCloneTarget(fs *serverfs.Filesystem, targetPath string) (gitCloneTargetMode, error) {
	info, err := fs.Stat(targetPath)
	if os.IsNotExist(err) {
		return gitCloneTargetEmpty, nil
	}
	if err != nil {
		return gitCloneTargetEmpty, fmt.Errorf("Could not inspect the clone target.")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return gitCloneTargetEmpty, fmt.Errorf("Clone target must not be a symlink.")
	}
	if !info.IsDir() {
		return gitCloneTargetEmpty, fmt.Errorf("Clone target already exists and is not a directory.")
	}

	entries, err := fs.ReadDirStat(targetPath)
	if err != nil {
		return gitCloneTargetEmpty, fmt.Errorf("Could not inspect the clone target.")
	}
	if len(entries) == 0 {
		return gitCloneTargetEmpty, nil
	}

	for _, entry := range entries {
		if !isAllowedGitCloneInternalEntry(entry) {
			return gitCloneTargetEmpty, fmt.Errorf("Clone target must be empty except for Better Files internal folders.")
		}
	}

	return gitCloneTargetInternalOnly, nil
}

func isAllowedGitCloneInternalEntry(entry os.FileInfo) bool {
	return entry.Name() == gitBetterFilesTrashDir && entry.IsDir()
}

func makeGitCloneTempTarget(fs *serverfs.Filesystem, containerTargetPath string) (string, string, error) {
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf(".betterfiles-git-clone-%d-%d", time.Now().UnixNano(), i)
		containerPath := path.Join(containerTargetPath, name)
		serverPath := containerPathToServerPath(containerPath)

		if _, err := fs.Stat(serverPath); os.IsNotExist(err) {
			return containerPath, serverPath, nil
		}
	}

	return "", "", fmt.Errorf("Could not allocate a temporary clone directory.")
}

func promoteGitCloneTempTarget(fs *serverfs.Filesystem, tempPath string, targetPath string) error {
	entries, err := fs.ReadDirStat(tempPath)
	if err != nil {
		return fmt.Errorf("Could not inspect the cloned repository.")
	}

	for _, entry := range entries {
		if _, err := fs.Stat(path.Join(targetPath, entry.Name())); err == nil {
			return fmt.Errorf("Clone output would overwrite %s.", entry.Name())
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("Could not inspect the clone output.")
		}
	}

	for _, entry := range entries {
		if err := fs.Rename(path.Join(tempPath, entry.Name()), path.Join(targetPath, entry.Name())); err != nil {
			return fmt.Errorf("Could not move cloned files into the target directory.")
		}
	}

	return nil
}

func addGitInternalExcludes(fs *serverfs.Filesystem, targetPath string) error {
	gitPath := path.Join(targetPath, ".git")
	infoPath := path.Join(gitPath, "info")
	excludePath := path.Join(infoPath, "exclude")

	stat, err := fs.Stat(gitPath)
	if err != nil {
		return err
	}
	if !stat.IsDir() {
		return fmt.Errorf("Git metadata path is not a directory.")
	}
	if err := fs.CreateDirectory("info", gitPath); err != nil {
		return err
	}

	content := ""
	if file, _, err := fs.File(excludePath); err == nil {
		raw, readErr := io.ReadAll(io.LimitReader(file, 64*1024))
		_ = file.Close()
		if readErr != nil {
			return readErr
		}
		content = string(raw)
	}

	var additions []string
	for _, line := range []string{gitBetterFilesTrashDir + "/", ".betterfiles-git-clone-*"} {
		if !strings.Contains(content, line) {
			additions = append(additions, line)
		}
	}
	if len(additions) == 0 {
		return nil
	}

	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += strings.Join(additions, "\n") + "\n"

	return fs.Write(excludePath, strings.NewReader(content), int64(len(content)), 0640)
}

func gitDiffArgs(cached bool, diffPath string) []string {
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--minimal"}
	if cached {
		args = append(args, "--cached")
	}

	args = append(args, "--")
	if diffPath != "" {
		args = append(args, diffPath)
	}

	return args
}

func combineGitDiffOutput(cached string, unstaged string) string {
	var sections []string
	if strings.TrimSpace(cached) != "" {
		sections = append(sections, "Staged changes:\n"+cached)
	}
	if strings.TrimSpace(unstaged) != "" {
		sections = append(sections, "Unstaged changes:\n"+unstaged)
	}

	output := strings.Join(sections, "\n")
	if len(output) > 512*1024 {
		output = output[:512*1024] + "\n...(truncated)"
	}

	return output
}

func isSafeGitRef(ref string) bool {
	if ref == "" || len(ref) > 255 || strings.HasPrefix(ref, "-") {
		return false
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "\\") {
		return false
	}
	return !strings.ContainsAny(ref, "\x00\n\r ~^:?*[")
}

func containerPathToServerPath(containerPath string) string {
	relative := strings.TrimPrefix(path.Clean(containerPath), gitContainerRoot)
	relative = strings.TrimPrefix(relative, "/")
	if relative == "" {
		return "/"
	}
	return "/" + relative
}

func gitLockFor(id string) *sync.Mutex {
	actual, _ := gitLocks.LoadOrStore(id, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

func maskGitResponse(result *gitResponse) *gitResponse {
	return &gitResponse{
		Stdout:   maskGitOutput(result.Stdout),
		Stderr:   maskGitOutput(result.Stderr),
		ExitCode: result.ExitCode,
	}
}

func maskGitOutput(output string) string {
	return gitCredentialPattern.ReplaceAllString(output, "${1}****@")
}

func isBlockedGitIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, block := range gitBlockedNetworks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func mustParseGitCIDR(ip string) *net.IPNet {
	_, block, err := net.ParseCIDR(ip)
	if err != nil {
		panic(fmt.Errorf("failed to parse Git CIDR: %s", ip))
	}
	return block
}

func getContainerUser() string {
	cfg := config.Get()
	if cfg.System.User.Rootless.Enabled {
		return fmt.Sprintf("%d:%d", cfg.System.User.Rootless.ContainerUID, cfg.System.User.Rootless.ContainerGID)
	}
	return strconv.Itoa(cfg.System.User.Uid) + ":" + strconv.Itoa(cfg.System.User.Gid)
}

func execInContainer(ctx context.Context, env *docker.Environment, cmd []string, workDir string) (*gitResponse, error) {
	cli := env.Client()

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		WorkingDir:   workDir,
		AttachStdout: true,
		AttachStderr: true,
		Env:          gitSafeEnv,
		User:         getContainerUser(),
	}

	exec, err := cli.ContainerExecCreate(ctx, env.Id, execConfig)
	if err != nil {
		return nil, err
	}

	resp, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{})
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
		done <- err
	}()

	select {
	case <-ctx.Done():
		return &gitResponse{Stderr: "command timed out", ExitCode: 124}, nil
	case err := <-done:
		if err != nil {
			return nil, err
		}
	}

	inspect, err := cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return nil, err
	}

	output := stdout.String()
	errOutput := stderr.String()

	if len(output) > 512*1024 {
		output = output[:512*1024] + "\n...(truncated)"
	}
	if len(errOutput) > 64*1024 {
		errOutput = errOutput[:64*1024] + "\n...(truncated)"
	}

	return &gitResponse{
		Stdout:   output,
		Stderr:   errOutput,
		ExitCode: inspect.ExitCode,
	}, nil
}
