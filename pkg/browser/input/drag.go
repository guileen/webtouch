package input

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseDragCoords 解析 ExecuteAction drag 的 value（"x1,y1,x2,y2" 起点→终点坐标，
// v1.5.1 追加语义，USER_ACTION.md #9）。容忍逗号/空白混合分隔；非法输入返回错误，
// 物理层不执行任何鼠标事件。
func ParseDragCoords(value string) (x1, y1, x2, y2 float64, err error) {
	fields := strings.FieldsFunc(strings.TrimSpace(value), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	if len(fields) != 4 {
		return 0, 0, 0, 0, fmt.Errorf("drag coords must be x1,y1,x2,y2 (got %q)", value)
	}
	nums := make([]float64, 4)
	for i, f := range fields {
		n, perr := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if perr != nil {
			return 0, 0, 0, 0, fmt.Errorf("drag coord %d invalid: %q", i+1, f)
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], nums[3], nil
}
