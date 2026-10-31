package recipes

import "embed"

// FS 内嵌所有标准站点的直发 Recipe JSON 声明。
//
//go:embed *.json
var FS embed.FS
