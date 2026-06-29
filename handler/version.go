package handler

import (
	"llmio/common"
	"llmio/consts"
	"github.com/gin-gonic/gin"
)

func GetVersion(c *gin.Context) {
	common.Success(c, consts.Version)
}
