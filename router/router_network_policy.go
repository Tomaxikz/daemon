package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/environment/docker"
	"github.com/pterodactyl/wings/internal/networkpolicy"
	"github.com/pterodactyl/wings/router/middleware"
)

func networkEnvironment(c *gin.Context) *docker.Environment {
	e, ok := middleware.ExtractServer(c).Environment.(*docker.Environment)
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "Network policies require Docker."})
		return nil
	}

	return e
}

func getNetworkCapabilities(c *gin.Context) {
	if e := networkEnvironment(c); e != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 7*time.Second)
		defer cancel()

		capabilities, err := e.NetworkCapabilities(ctx)
		if err != nil {
			networkError(c, err)
			return
		}

		c.JSON(http.StatusOK, capabilities)
	}
}

func getNetworkPolicy(c *gin.Context) {
	e := networkEnvironment(c)
	if e == nil {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 7*time.Second)
	defer cancel()

	status, err := e.NetworkPolicy(ctx)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, status)
}

func putNetworkPolicy(c *gin.Context) {
	e := networkEnvironment(c)
	if e == nil {
		return
	}

	var policy networkpolicy.Policy
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, networkpolicy.MaxBody))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Network policy request is too large."})
		} else {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Could not read policy."})
		}
		return
	}

	if err := networkpolicy.Decode(body, &policy); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Expected a complete, valid api_version 1 network policy envelope."})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 7*time.Second)
	defer cancel()

	_, err = e.UpdateNetworkPolicy(ctx, policy)
	if err != nil {
		networkError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"accepted": true})
}

func networkError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, docker.ErrNetworkInvalid):
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
	case errors.Is(err, docker.ErrNetworkConflict):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Network policy conflicts with the current revision or requires the server to be stopped.",
		})
	case errors.Is(err, docker.ErrNetworkUnsupported):
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "NSM enforcement is unavailable for this server's runtime or network mode."})
	default:
		middleware.CaptureAndAbort(c, err)
	}
}

func reconcileNetworkPolicy(c *gin.Context) {
	e := networkEnvironment(c)
	if e == nil {
		return
	}

	err := e.ReconcileNetwork(c.Request.Context())
	status, readErr := e.NetworkPolicy(c.Request.Context())
	if readErr != nil {
		middleware.CaptureAndAbort(c, readErr)
		return
	}
	if err != nil {
		networkError(c, err)
		return
	}

	c.JSON(http.StatusOK, status)
}
