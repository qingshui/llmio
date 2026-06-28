package balancers

import (
	"sync"
	"time"
)

// DefaultStickyTTL 是 sticky 缓存的默认过期时间。
const DefaultStickyTTL = 10 * time.Minute

type stickyEntry struct {
	key    uint
	expiry time.Time
}

var stickyCache sync.Map // key = modelKey + "|" + clientIP -> stickyEntry

// ResetStickyCache 清空包级 sticky 缓存，仅供测试使用。
func ResetStickyCache() {
	stickyCache = sync.Map{}
}

// StickyBalancer 在最外层（breaker 之外）包装内层 balancer，
// 使同一 model+clientIP 在 TTL 内尽量粘性到同一 provider。
type StickyBalancer struct {
	Balancer
	modelKey string
	clientIP string
	ttl      time.Duration
}

// NewSticky 构造 sticky balancer。ttl <= 0 时使用 DefaultStickyTTL。
func NewSticky(inner Balancer, modelKey, clientIP string, ttl time.Duration) *StickyBalancer {
	if ttl <= 0 {
		ttl = DefaultStickyTTL
	}
	return &StickyBalancer{Balancer: inner, modelKey: modelKey, clientIP: clientIP, ttl: ttl}
}

func (s *StickyBalancer) cacheKey() string {
	return s.modelKey + "|" + s.clientIP
}

func (s *StickyBalancer) loadValid() (uint, bool) {
	v, ok := stickyCache.Load(s.cacheKey())
	if !ok {
		return 0, false
	}
	entry := v.(stickyEntry)
	if !entry.expiry.After(time.Now()) {
		stickyCache.Delete(s.cacheKey())
		return 0, false
	}
	// 校验粘性 key 是否仍在内层候选池
	if h, ok := s.Balancer.(hasChecker); ok {
		if !h.Has(entry.key) {
			stickyCache.Delete(s.cacheKey())
			return 0, false
		}
	}
	return entry.key, true
}

func (s *StickyBalancer) store(key uint) {
	stickyCache.Store(s.cacheKey(), stickyEntry{key: key, expiry: time.Now().Add(s.ttl)})
}

func (s *StickyBalancer) Pop() (uint, error) {
	if key, ok := s.loadValid(); ok {
		return key, nil
	}
	id, err := s.Balancer.Pop()
	if err != nil {
		return 0, err
	}
	s.store(id)
	return id, nil
}

func (s *StickyBalancer) Delete(key uint) {
	s.Balancer.Delete(key)
	// 粘性目标失败，失效缓存，本请求后续重试重新选择
	if v, ok := stickyCache.Load(s.cacheKey()); ok {
		if v.(stickyEntry).key == key {
			stickyCache.Delete(s.cacheKey())
		}
	}
}

func (s *StickyBalancer) Reduce(key uint) {
	s.Balancer.Reduce(key)
	// 429 仅降权，不失效缓存：下次仍可能命中（由内层决定是否选中）
}

func (s *StickyBalancer) Success(key uint) {
	// 重试落到备用后，把粘性更新为最终成功的 provider 并续期
	s.store(key)
	s.Balancer.Success(key)
}
