package router

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/environment/docker"
	"github.com/pterodactyl/wings/router/middleware"
)

var allowedGitCommands = map[string]bool{
	"status":    true,
	"log":       true,
	"diff":      true,
	"show":      true,
	"branch":    true,
	"remote":    true,
	"rev-parse": true,
	"stash":     true,
	"add":       true,
	"commit":    true,
	"pull":      true,
	"push":      true,
	"fetch":     true,
	"checkout":  true,
	"reset":     true,
	"merge":     true,
	"rebase":    true,
	"tag":       true,
	"init":      true,
	"clone":     true,
}

var blockedGitFlags = []string{
	"--exec",
	"--upload-pack",
	"--receive-pack",
	"-c core.sshCommand",
	"--config core.sshCommand",
	"core.gitProxy",
	"credential.helper",
	"http.proxy",
}

type gitRequest struct {
	Subcommand string   `json:"subcommand" binding:"required"`
	Args       []string `json:"args"`
	WorkDir    string   `json:"work_dir"`
}

type gitResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func postServerGit(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var req gitRequest
	if err := c.BindJSON(&req); err != nil {
		return
	}

	req.Subcommand = strings.TrimSpace(req.Subcommand)
	if !allowedGitCommands[req.Subcommand] {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "This git subcommand is not allowed.",
		})
		return
	}

	fullArgs := strings.Join(req.Args, " ")
	for _, blocked := range blockedGitFlags {
		if strings.Contains(strings.ToLower(fullArgs), strings.ToLower(blocked)) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "This git flag is not allowed for security reasons.",
			})
			return
		}
	}

	for _, arg := range req.Args {
		if strings.Contains(arg, ";") || strings.Contains(arg, "|") ||
			strings.Contains(arg, "&") || strings.Contains(arg, "`") ||
			strings.Contains(arg, "$(") || strings.Contains(arg, "\n") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Shell metacharacters are not allowed in arguments.",
			})
			return
		}
	}

	env, ok := s.Environment.(*docker.Environment)
	if !ok {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Git operations are only supported in Docker environments.",
		})
		return
	}

	workDir := "/home/container"
	if req.WorkDir != "" {
		cleaned := strings.TrimSpace(req.WorkDir)
		if !strings.HasPrefix(cleaned, "/home/container") {
			cleaned = "/home/container/" + strings.TrimPrefix(cleaned, "/")
		}
		if strings.Contains(cleaned, "..") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Path traversal is not allowed.",
			})
			return
		}
		workDir = cleaned
	}

	cmd := []string{"git", req.Subcommand}
	cmd = append(cmd, req.Args...)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	result, err := execInContainer(ctx, env, cmd, workDir)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
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

	workDir := "/home/container"
	if wd := c.Query("work_dir"); wd != "" {
		workDir = sanitizeWorkDir(wd)
		if workDir == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Invalid work directory."})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	checkResult, err := execInContainer(ctx, env, []string{"git", "rev-parse", "--is-inside-work-tree"}, workDir)
	if err != nil || checkResult.ExitCode != 0 {
		c.JSON(http.StatusOK, gin.H{
			"available": false,
			"is_repo":   false,
		})
		return
	}

	statusResult, _ := execInContainer(ctx, env, []string{"git", "status", "--porcelain", "-b"}, workDir)
	branchResult, _ := execInContainer(ctx, env, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, workDir)
	remoteResult, _ := execInContainer(ctx, env, []string{"git", "remote", "-v"}, workDir)
	logResult, _ := execInContainer(ctx, env, []string{"git", "log", "--oneline", "-20"}, workDir)

	c.JSON(http.StatusOK, gin.H{
		"available": true,
		"is_repo":   true,
		"branch":    strings.TrimSpace(branchResult.Stdout),
		"status":    statusResult.Stdout,
		"remotes":   remoteResult.Stdout,
		"log":       logResult.Stdout,
	})
}

func sanitizeWorkDir(wd string) string {
	cleaned := strings.TrimSpace(wd)
	if strings.Contains(cleaned, "..") {
		return ""
	}
	if !strings.HasPrefix(cleaned, "/home/container") {
		cleaned = "/home/container/" + strings.TrimPrefix(cleaned, "/")
	}
	return cleaned
}

func execInContainer(ctx context.Context, env *docker.Environment, cmd []string, workDir string) (*gitResponse, error) {
	cli := env.Client()

	execConfig := types.ExecConfig{
		Cmd:          cmd,
		WorkingDir:   workDir,
		AttachStdout: true,
		AttachStderr: true,
		User:         "container",
	}

	exec, err := cli.ContainerExecCreate(ctx, env.Id, execConfig)
	if err != nil {
		return nil, err
	}

	resp, err := cli.ContainerExecAttach(ctx, exec.ID, types.ExecStartCheck{})
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&stdout, resp.Reader)
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
