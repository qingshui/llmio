package handler

import (
	"github.com/gin-gonic/gin"
	"llmio/common"
	"llmio/consts"
)

func GetVersion(c *gin.Context) {
	common.Success(c, consts.Version)
}
