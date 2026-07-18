// capability.go 承载 fusion_capable 推导（SC-4② 重释义=driver capability 标志）：融合能力
// 从既有 NodeInfo.tools / NodeSession.Tools **纯推导**，无新 proto 字段——node.proto 与 MCP
// 工具面零动。M6 能力子集过滤（driver supports_tool 过滤 Register.tools）已保证 tools 是
// driver 真实能力子集，故 controller 侧以「⊇ 融合前置工具集」单点判定即等价 driver 通告。
package fusion

import "slices"

// fusionRequiredTools 是融合的两个前置 node 快工具：原子采集（engine.collect）恰好
// back-to-back 下发这两个工具，缺一融合无从执行。
var fusionRequiredTools = []string{"screenshot", "get_a11y_tree"}

// FusionCapable 判定节点是否具备融合前置能力：tools ⊇ {screenshot, get_a11y_tree}。
// 入参为节点通告的能力子集（registry.NodeSession.Tools / NodeInfo.tools）；空/nil 即 false。
func FusionCapable(tools []string) bool {
	for _, req := range fusionRequiredTools {
		if !slices.Contains(tools, req) {
			return false
		}
	}
	return true
}
