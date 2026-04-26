package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
)

func postServerImport(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		User               string `json:"user"`
		Password           string `json:"password"`
		SshKey             string `json:"ssh_key"`
		SshKeyPassphrase   string `json:"ssh_key_passphrase"`
		HostKeyFingerprint string `json:"host_key_fingerprint"`
		Hote               string `json:"hote"`
		Port               int    `json:"port"`
		Srclocation        string `json:"srclocation"`
		Dstlocation        string `json:"dstlocation"`
		Wipe               bool   `json:"wipe"`
		Type               string `json:"type"`
		AuthMethod         string `json:"auth_method"`
	}

	if err := c.BindJSON(&data); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if s.ExecutingPowerAction() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Cannot execute server import while another power action is running.",
		})
		return
	}

	go func(srv *server.Server) {
		if err := srv.ImportNew(data.User, data.Password, data.SshKey, data.SshKeyPassphrase, data.HostKeyFingerprint, data.Hote, data.Port, data.Srclocation, data.Dstlocation, data.Type, data.AuthMethod, data.Wipe); err != nil {
			srv.Log().WithField("error", err).Error("failed to complete server import process")
		}
	}(s)

	c.Status(http.StatusAccepted)
}

func postServerImportSelected(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		User               string   `json:"user"`
		Password           string   `json:"password"`
		SshKey             string   `json:"ssh_key"`
		SshKeyPassphrase   string   `json:"ssh_key_passphrase"`
		HostKeyFingerprint string   `json:"host_key_fingerprint"`
		Hote               string   `json:"hote"`
		Port               int      `json:"port"`
		Srclocation        string   `json:"srclocation"`
		Dstlocation        string   `json:"dstlocation"`
		Wipe               bool     `json:"wipe"`
		Type               string   `json:"type"`
		AuthMethod         string   `json:"auth_method"`
		SelectedItems      []string `json:"selected_items"`
	}

	if err := c.BindJSON(&data); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if s.ExecutingPowerAction() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Cannot execute server import while another power action is running.",
		})
		return
	}

	go func(srv *server.Server) {
		if err := srv.ImportNewSelected(data.User, data.Password, data.SshKey, data.SshKeyPassphrase, data.HostKeyFingerprint, data.Hote, data.Port, data.Srclocation, data.Dstlocation, data.Type, data.AuthMethod, data.Wipe, data.SelectedItems); err != nil {
			srv.Log().WithField("error", err).Error("failed to complete selective server import process")
		}
	}(s)

	c.Status(http.StatusAccepted)
}

func getServerImporterProgress(c *gin.Context) {
	s := ExtractServer(c)

	progress, ok := s.GetImportProgressSnapshot()
	if !ok {
		c.JSON(http.StatusOK, gin.H{
			"is_importing": false,
			"progress":     nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"is_importing": true,
		"progress":     progress,
	})
}
