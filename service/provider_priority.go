package service

const providerPriorityBoostBase = 1 << 30

// boostProviderPriority 计算某条模型-Provider关联在 AuthKey 有序 provider 列表作用下的选桶优先级。
//   - 列表内第 i 个 provider → boostBase - i（数字越大越优先，同 provider 多条关联同层，层内按 Weight 加权）
//   - 列表外 provider → min(原Priority, boostBase - len - 1)，保证排在列表内全部 provider 之后
//   - 列表为空 → 原样返回
func boostProviderPriority(providerID uint, originalPriority int, providerPriorities []uint) int {
	if len(providerPriorities) == 0 {
		return originalPriority
	}
	for i, pid := range providerPriorities {
		if pid == providerID {
			return providerPriorityBoostBase - i
		}
	}
	return min(originalPriority, providerPriorityBoostBase-len(providerPriorities)-1)
}
