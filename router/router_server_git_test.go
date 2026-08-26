package router

import (
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

func TestGitBaseCommandDisablesHTTPRedirects(t *testing.T) {
	command := gitBaseCommand("git")

	require.Contains(t, command, "http.followRedirects=false")
	require.Contains(t, command, "remote.origin.proxy=")
	require.Contains(t, command, "core.hooksPath=/dev/null")
	require.Contains(t, command, "fetch.bundleURI=")
	require.Contains(t, command, "transfer.bundleURI=false")
	require.Contains(t, command, "core.alternateRefsCommand=/bin/true")
	require.Contains(t, command, "gc.auto=0")
	require.Contains(t, command, "maintenance.auto=false")
}

func TestGitVersionSupportsPinnedResolution(t *testing.T) {
	tests := map[string]bool{
		"git version 2.36.9":           false,
		"git version 2.37":             true,
		"git version 2.37.0":           true,
		"git version 2.37.0.windows.1": true,
		"git version 3.0.0":            true,
		"git version 1.99.9":           false,
		"git version 2.37invalid":      false,
		"not a git version":            false,
	}

	for version, expected := range tests {
		t.Run(version, func(t *testing.T) {
			require.Equal(t, expected, gitVersionSupportsPinnedResolution(version))
		})
	}
}

func TestGitUnsafeLocalConfigPolicyRejectsHTTPSettings(t *testing.T) {
	pattern := regexp.MustCompile(gitUnsafeLocalConfigPattern)

	for _, key := range []string{
		"http.followRedirects",
		"http.proxy",
		"http.https://git.example.com/repository.git.followRedirects",
		"http.https://git.example.com/repository.git.curloptResolve",
		"remote.origin.proxy",
		"remote.origin.proxyauthmethod",
		"remote.origin.vcs",
		"remote.origin.promisor",
		"remote.origin.partialCloneFilter",
		"extensions.partialClone",
		"core.alternateRefsCommand",
		"Core.SSHCOMMAND",
		"fetch.bundleURI",
		"transfer.bundleURI",
		"gc.auto",
		"maintenance.auto",
		"filter.assets.process",
		"url.https://git.example.com/.insteadOf",
	} {
		require.Truef(t, pattern.MatchString(key), "expected %q to be rejected", key)
	}

	for _, key := range []string{"user.name", "remote.origin.url", "branch.main.merge"} {
		require.Falsef(t, pattern.MatchString(key), "expected %q to remain allowed", key)
	}
}

func TestGitCommandPinsExactRemoteAndDisablesProxyAndRedirects(t *testing.T) {
	remote := validatedGitRepositoryURL{
		URL:            "https://git.example.com/owner/repository.git",
		CurlOptResolve: "git.example.com:443:8.8.8.8",
		RemoteName:     "8e6df22c742447f39e91a18de42f28c646b080226ebe642f",
	}
	command := gitCommand("git", []string{"ls-remote", remote.RemoteName}, remote)
	alias := gitRemoteAlias(remote.RemoteName)

	requireGitCommandConfig(t, command, "url."+remote.URL+".insteadOf="+alias)
	requireGitCommandConfig(t, command, "remote."+remote.RemoteName+".url="+alias)
	requireGitCommandConfig(t, command, "remote."+remote.RemoteName+".proxy=")

	for _, requestURL := range []string{
		remote.URL,
		remote.URL + "/",
	} {
		prefix := "http." + requestURL + "."
		requireGitCommandConfig(t, command, prefix+"followRedirects=false")
		requireGitCommandConfig(t, command, prefix+"proxy=")
		requireGitCommandConfig(t, command, prefix+"curloptResolve=")
		requireGitCommandConfig(t, command, prefix+"curloptResolve="+remote.CurlOptResolve)
	}
	require.Equal(t, []string{"ls-remote", remote.RemoteName}, command[len(command)-2:])
}

func TestGitRemoteNameIsUnpredictableAndConfigSafe(t *testing.T) {
	first, err := newGitRemoteName()
	require.NoError(t, err)
	second, err := newGitRemoteName()
	require.NoError(t, err)

	require.Regexp(t, `^[0-9a-f]{48}$`, first)
	require.Regexp(t, `^[0-9a-f]{48}$`, second)
	require.NotEqual(t, first, second)
	require.Equal(t, first+":", gitRemoteAlias(first))
}

func TestHideGitRemoteIdentities(t *testing.T) {
	remote := validatedGitRepositoryURL{RemoteName: "8e6df22c742447f39e91a18de42f28c646b080226ebe642f"}
	result := &gitResponse{
		Stdout: "fetching " + remote.RemoteName,
		Stderr: "failed " + gitRemoteAlias(remote.RemoteName),
	}

	hideGitRemoteIdentities(result, []validatedGitRepositoryURL{remote})
	require.NotContains(t, result.Stdout, remote.RemoteName)
	require.NotContains(t, result.Stderr, remote.RemoteName)
	require.Contains(t, result.Stdout, "[remote]")
	require.Contains(t, result.Stderr, "[remote]")
}

func TestGitOpaqueRemoteCannotBeReplacedByKnownLocalMappings(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}

	repository := t.TempDir()
	runGitTestCommand(t, repository, gitBin, "init", "--quiet")

	validatedURL := "https://git.example.com/owner/repository.git"
	maliciousURL := "https://127.0.0.1/internal.git"
	runGitTestCommand(t, repository, gitBin, "config", "--local", "url."+maliciousURL+".insteadOf", validatedURL)
	runGitTestCommand(t, repository, gitBin, "config", "--local", "url.https://127.0.0.2/.insteadOf", "https://")
	runGitTestCommand(t, repository, gitBin, "config", "--local", "remote."+validatedURL+".url", maliciousURL)
	runGitTestCommand(t, repository, gitBin, "config", "--local", "remote.origin.url", maliciousURL)

	remote, err := newValidatedGitRepositoryURL(validatedURL, "git.example.com:443:8.8.8.8")
	require.NoError(t, err)
	command := gitCommand(gitBin, []string{"remote", "-v"}, remote)
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = repository
	cmd.Env = append(os.Environ(), gitSafeEnv...)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	remoteOutput := strings.TrimSpace(string(output))
	require.Contains(t, remoteOutput, remote.RemoteName+"\t"+validatedURL+" (fetch)")
	require.NotContains(t, remoteOutput, remote.RemoteName+"\t"+maliciousURL)
}

func TestGitFetchDoesNotRunWorkingTreeFilters(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	falseBin, err := exec.LookPath("false")
	if err != nil {
		t.Skip("a failing filter command is not available")
	}

	source := t.TempDir()
	runGitTestCommand(t, source, gitBin, "init", "--quiet", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(source, ".gitattributes"), []byte("*.txt filter=race\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "data.txt"), []byte("first\n"), 0o600))
	runGitTestCommand(t, source, gitBin, "add", ".")
	runGitTestCommand(t, source, gitBin, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")

	cloneParent := t.TempDir()
	clone := filepath.Join(cloneParent, "clone")
	runGitTestCommand(t, cloneParent, gitBin, "clone", "--quiet", source, clone)
	runGitTestCommand(t, clone, gitBin, "config", "--local", "filter.race.required", "true")
	runGitTestCommand(t, clone, gitBin, "config", "--local", "filter.race.smudge", falseBin)

	require.NoError(t, os.WriteFile(filepath.Join(source, "data.txt"), []byte("second\n"), 0o600))
	runGitTestCommand(t, source, gitBin, "add", "data.txt")
	runGitTestCommand(t, source, gitBin, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "update")

	// Fetch only transfers objects and updates FETCH_HEAD; it must not invoke the
	// deliberately failing worktree filter. Checkout proves the filter is armed.
	remote, err := newValidatedGitRepositoryURL(source, "")
	require.NoError(t, err)
	fetchArgs := append([]string{"-c", "protocol.file.allow=always", "--no-pager"}, gitPullFetchArgs(remote, "main")...)
	command := gitCommand(gitBin, fetchArgs, remote)
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = clone
	cmd.Env = gitTestEnvironment()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	runGitTestCommandMustFail(t, clone, gitBin, "checkout", "FETCH_HEAD", "--", "data.txt")
}

func TestGitCloneFinalizationPersistsValidatedURL(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}

	source := t.TempDir()
	runGitTestCommand(t, source, gitBin, "init", "--quiet", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(source, "README.md"), []byte("test\n"), 0o600))
	runGitTestCommand(t, source, gitBin, "add", "README.md")
	runGitTestCommand(t, source, gitBin, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")

	remote, err := newValidatedGitRepositoryURL("https://git.example.com/owner/repository.git", "git.example.com:443:8.8.8.8")
	require.NoError(t, err)
	cloneParent := t.TempDir()
	clone := filepath.Join(cloneParent, "clone")
	alias := gitRemoteAlias(remote.RemoteName)
	runGitTestCommand(t, cloneParent, gitBin,
		"-c", "url."+source+".insteadOf="+alias,
		"clone", "--quiet", "--no-checkout", alias, clone,
	)
	runGitTestCommand(t, clone, gitBin, gitClonePersistRemoteArgs(remote)...)
	runGitTestCommand(t, clone, gitBin, gitCloneCheckoutArgs()...)

	config, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
	require.NoError(t, err)
	require.Contains(t, string(config), remote.URL)
	require.NotContains(t, string(config), alias)
	require.NotContains(t, string(config), remote.RemoteName)
	_, err = os.Stat(filepath.Join(clone, "README.md"))
	require.NoError(t, err)
}

func TestGitFetchThenFastForwardMerge(t *testing.T) {
	for _, test := range []struct {
		name    string
		shallow bool
	}{
		{name: "shallow clone", shallow: true},
		{name: "complete clone", shallow: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			testGitFetchThenFastForwardMerge(t, test.shallow)
		})
	}
}

func testGitFetchThenFastForwardMerge(t *testing.T, shallow bool) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}

	source := t.TempDir()
	runGitTestCommand(t, source, gitBin, "init", "--quiet", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(source, "data.txt"), []byte("first\n"), 0o600))
	runGitTestCommand(t, source, gitBin, "add", "data.txt")
	runGitTestCommand(t, source, gitBin, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")

	remoteURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(source)}).String()
	cloneParent := t.TempDir()
	clone := filepath.Join(cloneParent, "clone")
	cloneArgs := []string{"-c", "protocol.file.allow=always", "clone", "--quiet"}
	if shallow {
		cloneArgs = append(cloneArgs, "--depth=1")
	}
	cloneArgs = append(cloneArgs, remoteURL, clone)
	runGitTestCommand(t, cloneParent, gitBin, cloneArgs...)

	for _, content := range []string{"second\n", "third\n", "fourth\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(source, "data.txt"), []byte(content), 0o600))
		runGitTestCommand(t, source, gitBin, "add", "data.txt")
		runGitTestCommand(t, source, gitBin, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", strings.TrimSpace(content))
	}

	remote, err := newValidatedGitRepositoryURL(remoteURL, "")
	require.NoError(t, err)
	fetchArgs := append([]string{"-c", "protocol.file.allow=always"}, gitPullFetchArgs(remote, "main")...)
	runGitCommandVector(t, clone, gitCommand(gitBin, fetchArgs, remote))
	runGitCommandVector(t, clone, gitCommand(gitBin, gitPullMergeArgs()))

	data, err := os.ReadFile(filepath.Join(clone, "data.txt"))
	require.NoError(t, err)
	require.Equal(t, "fourth\n", string(data))
}

func runGitTestCommand(t *testing.T, workDir string, gitBin string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = workDir
	cmd.Env = gitTestEnvironment()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func runGitTestCommandMustFail(t *testing.T, workDir string, gitBin string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = workDir
	cmd.Env = gitTestEnvironment()
	output, err := cmd.CombinedOutput()
	require.Error(t, err, string(output))
}

func runGitCommandVector(t *testing.T, workDir string, command []string) {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = workDir
	cmd.Env = gitTestEnvironment()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitTestEnvironment() []string {
	return append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
}

func TestGitRemoteConfigURLsCoverNormalizedTrailingSlash(t *testing.T) {
	require.Equal(t, []string{
		"https://git.example.com/owner/repository.git",
		"https://git.example.com/owner/repository.git/",
	}, gitRemoteConfigURLs("https://git.example.com/owner/repository.git"))
	require.Equal(t, []string{
		"https://git.example.com/owner/repository.git/",
	}, gitRemoteConfigURLs("https://git.example.com/owner/repository.git/"))
}

func TestGitNetworkAndOfflineCommandShapes(t *testing.T) {
	remote := validatedGitRepositoryURL{
		URL:        "https://git.example.com/owner/repository.git",
		RemoteName: "8e6df22c742447f39e91a18de42f28c646b080226ebe642f",
	}

	clone := gitCloneNetworkArgs(remote, "/home/container/repository")
	require.Equal(t, "clone", clone[0])
	require.Contains(t, clone, "--no-checkout")
	require.Contains(t, clone, gitRemoteAlias(remote.RemoteName))
	require.NotContains(t, clone, remote.URL)

	persist := gitClonePersistRemoteArgs(remote)
	require.Equal(t, []string{"config", "--local", "--replace-all", "remote.origin.url", remote.URL}, persist)
	require.NotContains(t, persist, gitRemoteAlias(remote.RemoteName))
	require.Equal(t, []string{"checkout", "--force"}, gitCloneCheckoutArgs())

	fetch := gitPullFetchArgs(remote, "main")
	require.Equal(t, "fetch", fetch[0])
	require.Contains(t, fetch, "--deepen=1")
	require.Contains(t, fetch, "--no-recurse-submodules")
	require.Equal(t, []string{remote.RemoteName, "main"}, fetch[len(fetch)-2:])
	require.NotContains(t, fetch, "pull")

	merge := gitPullMergeArgs()
	require.Equal(t, []string{"merge", "--ff-only", "--no-edit", "FETCH_HEAD"}, merge)
	require.Equal(t, container.NetworkMode("none"), gitHelperNetworkMode(container.NetworkMode("bridge"), true))
	require.Equal(t, container.NetworkMode("bridge"), gitHelperNetworkMode(container.NetworkMode("bridge"), false))
}

func TestCombineGitResponsesUsesFinalExitCode(t *testing.T) {
	combined := combineGitResponses(
		&gitResponse{Stdout: "fetch stdout\n", Stderr: "fetch stderr\n", ExitCode: 0},
		&gitResponse{Stdout: "merge stdout\n", Stderr: "merge stderr\n", ExitCode: 1},
	)
	require.Equal(t, "fetch stdout\nmerge stdout\n", combined.Stdout)
	require.Equal(t, "fetch stderr\nmerge stderr\n", combined.Stderr)
	require.Equal(t, 1, combined.ExitCode)
}

func TestGitSafeEnvironmentClearsConfigAndProxyInjection(t *testing.T) {
	for _, setting := range []string{
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_PARAMETERS=",
		"GIT_NO_LAZY_FETCH=1",
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"all_proxy=",
	} {
		require.Contains(t, gitSafeEnv, setting)
	}
}

func TestGitCanonicalRepositoryURLEscapesCommandConfigDelimiter(t *testing.T) {
	parsed, err := url.Parse("https://git.example.com/owner/repository=name.git")
	require.NoError(t, err)
	require.Equal(t, "https://git.example.com/owner/repository%3Dname.git", gitCanonicalRepositoryURL(parsed))

	alreadyEscaped, err := url.Parse("https://git.example.com/owner/repository%3Dname.git")
	require.NoError(t, err)
	require.Equal(t, "https://git.example.com/owner/repository%3Dname.git", gitCanonicalRepositoryURL(alreadyEscaped))
}

func requireGitCommandConfig(t *testing.T, command []string, expected string) {
	t.Helper()
	for index := 1; index < len(command); index++ {
		if command[index-1] == "-c" && command[index] == expected {
			return
		}
	}
	t.Fatalf("Git command does not contain config %q: %#v", expected, command)
}

func TestGitCurlOptResolvePinsValidatedAddress(t *testing.T) {
	tests := map[string]struct {
		host     string
		port     string
		ip       net.IP
		expected string
	}{
		"default HTTPS port": {
			host:     "git.example.com",
			ip:       net.ParseIP("8.8.8.8"),
			expected: "git.example.com:443:8.8.8.8",
		},
		"explicit port": {
			host:     "git.example.com",
			port:     "8443",
			ip:       net.ParseIP("8.8.8.8"),
			expected: "git.example.com:8443:8.8.8.8",
		},
		"IPv6 address": {
			host:     "git.example.com",
			ip:       net.ParseIP("2001:4860:4860::8888"),
			expected: "git.example.com:443:[2001:4860:4860::8888]",
		},
		"IPv6 literal host": {
			host:     "2001:4860:4860::8888",
			ip:       net.ParseIP("2001:4860:4860::8888"),
			expected: "[2001:4860:4860::8888]:443:[2001:4860:4860::8888]",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, test.expected, gitCurlOptResolve(test.host, test.port, test.ip))
		})
	}
}
