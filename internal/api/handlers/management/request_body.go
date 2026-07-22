package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

const (
	maxManagementAuthUploadBodyBytes = 32 << 20
	maxVertexCredentialBodyBytes     = 8 << 20
	maxManagementConfigBodyBytes     = 8 << 20
	maxManagementJSONBodyBytes       = 2 << 20
)

func readManagementRequestBody(c *gin.Context, maxBytes int64) ([]byte, error) {
	return sdkhandlers.ReadRequestBodyWithLimits(c, maxBytes, maxBytes)
}

func decodeManagementJSONBody(c *gin.Context, maxBytes int64, target any) error {
	body, err := readManagementRequestBody(c, maxBytes)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if errDecode := decoder.Decode(target); errDecode != nil {
		return errDecode
	}
	var trailing any
	if errDecode := decoder.Decode(&trailing); errDecode != io.EOF {
		if errDecode == nil {
			return fmt.Errorf("request body must contain exactly one JSON value")
		}
		return errDecode
	}
	return nil
}

func writeManagementRequestBodyError(c *gin.Context, err error) {
	if sdkhandlers.IsRequestBodyTooLarge(err) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":   "request_too_large",
			"message": "request body exceeds the allowed size",
		})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request body: %v", err)})
}
