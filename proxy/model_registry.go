package proxy

import (
	"strings"
	"sync"

	"kiro-go/config"
)

// ==================== 动态模型元数据注册表 ====================
//
// Kiro 的 ListAvailableModels 为每个模型透出两类权威信息:
//   - tokenLimits.{maxInputTokens, maxOutputTokens} —— 真实上下文窗口与输出上限
//   - additionalModelRequestFieldsSchema —— 该模型真实支持的 effort/reasoning 档位
//     及其承载路径(output_config / reasoning)
//
// 刷新模型缓存时(refreshModelsCache / fetchAndCacheAccountModels)把它们登记到这里,
// 供三处消费:getContextWindowSize、modelMaxOutputTokens、buildAdditionalModelRequestFields。
// 这样新模型上线(如 claude-opus-5)不用改代码就能拿到正确的窗口、输出上限与思考档位;
// 冷启动或回源失败时才回落到按版本号推断的家族兜底(见 isLargeContextModel /
// fallbackSchemaPath)。
//
// 设计对齐 Rust 版 kiro2cc-proxy 的 model_registry(converter.rs)。

type modelMeta struct {
	maxInputTokens  int
	maxOutputTokens int
	// effortSchemaPath 为 "output_config" / "reasoning";空串表示该模型**不支持**
	// additionalModelRequestFields(发了会被上游 400)。已登记的模型以此为准。
	effortSchemaPath string
	effortLevels     []string
	defaultEffort    string
}

var (
	modelMetaMu       sync.RWMutex
	modelMetaRegistry = map[string]modelMeta{}
)

// registerModelMeta 登记一批模型的权威元数据(按 kiro_id 覆盖)。
// 空列表不清空注册表,避免上游偶发空返回把已登记信息抹掉。
func registerModelMeta(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	modelMetaMu.Lock()
	defer modelMetaMu.Unlock()
	for i := range models {
		m := &models[i]
		key := strings.ToLower(strings.TrimSpace(m.ModelId))
		if key == "" {
			continue
		}
		meta := modelMeta{}
		if m.TokenLimits != nil {
			meta.maxInputTokens = m.TokenLimits.MaxInputTokens
			meta.maxOutputTokens = m.TokenLimits.MaxOutputTokens
		}
		if eff := m.EffortInfo(); eff != nil {
			meta.effortSchemaPath = eff.SchemaPath
			meta.effortLevels = eff.Levels
			meta.defaultEffort = eff.DefaultLevel
		}
		modelMetaRegistry[key] = meta
		// 同时登记连字符写法(claude-opus-4-8):客户端/插件常直接传这种别名,
		// 而 Kiro 后端 id 是点分格式。
		if dashed := strings.ReplaceAll(key, ".", "-"); dashed != key {
			modelMetaRegistry[dashed] = meta
		}
	}
}

// lookupModelMeta 查模型的权威元数据。入参可为 kiro_id、连字符别名,或带 thinking
// 后缀的名字(后缀会被剥掉)。未登记返回 false,调用方需自行按家族兜底。
func lookupModelMeta(model string) (modelMeta, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return modelMeta{}, false
	}
	if suffix := strings.ToLower(config.GetThinkingConfig().Suffix); suffix != "" {
		key = strings.TrimSuffix(key, suffix)
	}

	modelMetaMu.RLock()
	defer modelMetaMu.RUnlock()
	if meta, ok := modelMetaRegistry[key]; ok {
		return meta, true
	}
	// 点分 ↔ 连字符互查一次(注册表两种写法都登记了,这里只兜非常规输入)。
	if meta, ok := modelMetaRegistry[strings.ReplaceAll(key, ".", "-")]; ok {
		return meta, true
	}
	return modelMeta{}, false
}

// effortAllowedByModel 判断某 effort 档位是否被该模型接受。
// 仅当注册表登记了该模型的合法档位时才做判定;未登记一律放行(交给已知超集校验)。
func effortAllowedByModel(model string, effort string) (allowed bool, known bool) {
	meta, ok := lookupModelMeta(model)
	if !ok || len(meta.effortLevels) == 0 {
		return true, false
	}
	for _, lv := range meta.effortLevels {
		if strings.EqualFold(lv, effort) {
			return true, true
		}
	}
	return false, true
}
