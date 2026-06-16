package router

import "github.com/gin-gonic/gin"

// Handles streaming a specific file with HTTP Range support.
func getDownloadStream(c *gin.Context) {
	df, ok := getTokenDownloadFile(c)
	if !ok {
		return
	}
	defer df.Close()

	serveDownloadFile(c, df, "inline", true, false)
}
