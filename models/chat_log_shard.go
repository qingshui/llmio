package models

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// chat_logs 按月分表存储核心层。
//
//   - 表名 chat_logs_YYYYMM（本地时区月份）；base 表 chat_logs 已废弃为 legacy，
//     不再由 AutoMigrate 维护、不再被业务读写。
//   - id 全局连续：新分表创建时接续全局 max(id)+1，保证 id 升序=时间升序，
//     跨分表可直接按 id DESC 归并，水位线增量只需扫当前月新增段。
//   - 注册表 chat_log_shards 只记录「已封存」月份的 id 区间与行数；
//     当前月隐式（不入注册表）。月份切换（插入时发现新月）对上月封存。
//   - 切月竞态：插入在 RLock 临界区内执行；切月封存持写锁等在途插入排空后
//     再统计，避免注册表少算。残余竞态由 id 路由的逐表探测兜底。
//   - 单实例假设：惰性建表/封存/进程内缓存按单写实例设计（与日志页 SWR
//     缓存、总数水位线一致），不支持多实例部署。
const (
	chatLogShardTablePrefix = "chat_logs_"
	chatLogMonthKeyLayout   = "200601"
)

var chatLogShardTableRe = regexp.MustCompile(`^chat_logs_\d{6}$`)

// chatLogShardNow 当前时间取值，测试中替换以模拟跨月
var chatLogShardNow = time.Now

// ChatLogShard 已封存月份的注册行：min/max id 区间与行数在封存时一次性冻结。
type ChatLogShard struct {
	MonthKey  string    `gorm:"column:month_key;size:7;primaryKey"`
	MinID     uint      `gorm:"column:min_id"`
	MaxID     uint      `gorm:"column:max_id"`
	RowsCount int64     `gorm:"column:rows_count"`
	SealedAt  time.Time `gorm:"column:sealed_at"`
}

// ChatLogShardRef 窗口查询中的一个分表。Boundary=true 表示该月仅部分
// 落在 [cutoff, now) 内，查询需附加 created_at >= cutoff 谓词；
// false 表示整月都在窗口内，无需时间谓词（分表本身就是月区间）。
type ChatLogShardRef struct {
	MonthKey string
	Table    string
	Boundary bool
}

type chatLogShardState struct {
	mu           sync.RWMutex
	loaded       bool
	ready        bool
	currentMonth string
	sealed       []ChatLogShard // 按 MonthKey 降序（新→旧）
}

var chatLogShardsV chatLogShardState

func ChatLogMonthKey(t time.Time) string {
	return t.Format(chatLogMonthKeyLayout)
}

func CurrentChatLogMonthKey() string {
	return ChatLogMonthKey(chatLogShardNow())
}

func ChatLogShardTableName(monthKey string) string {
	return chatLogShardTablePrefix + monthKey
}

// CurrentChatLogShardTable 当前月分表名。调用方应先经 WithChatLogShardForInsert /
// ChatLogShardsForWindow / LocateChatLogTable 等入口确保分表状态已就绪。
func CurrentChatLogShardTable() string {
	return ChatLogShardTableName(CurrentChatLogMonthKey())
}

func chatLogMonthStart(monthKey string) (time.Time, error) {
	return time.ParseInLocation(chatLogMonthKeyLayout, monthKey, time.Local)
}

// ChatLogMonthStart 指定月份（YYYYMM）的本地时区起点。
func ChatLogMonthStart(monthKey string) (time.Time, error) {
	return chatLogMonthStart(monthKey)
}

// WithChatLogShardForInsert 在当前月分表上执行写入。回调在读锁临界区内执行：
// 切月封存拿写锁时等待所有在途插入提交完再统计，保证注册表行数精确。
func WithChatLogShardForInsert(ctx context.Context, fn func(tx *gorm.DB, table string) error) error {
	st := &chatLogShardsV
	st.mu.RLock()
	if st.ready && st.currentMonth == ChatLogMonthKey(chatLogShardNow()) {
		err := fn(DB.WithContext(ctx), ChatLogShardTableName(st.currentMonth))
		st.mu.RUnlock()
		return err
	}
	st.mu.RUnlock()

	if err := ensureCurrentShard(ctx, ChatLogMonthKey(chatLogShardNow())); err != nil {
		return err
	}
	// 准备完成后重走快路径；月份恰在两次调用间跨界的极端情况会再次进入
	// 慢路径，prepare 幂等，递归必然收敛
	return WithChatLogShardForInsert(ctx, fn)
}

func ensureCurrentShard(ctx context.Context, month string) error {
	st := &chatLogShardsV
	st.mu.RLock()
	if st.ready && st.currentMonth == month {
		st.mu.RUnlock()
		return nil
	}
	st.mu.RUnlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ready && st.currentMonth == month {
		return nil
	}
	return prepareCurrentShardLocked(ctx, month)
}

func prepareCurrentShardLocked(ctx context.Context, month string) error {
	st := &chatLogShardsV
	if !st.loaded {
		if err := loadSealedLocked(ctx); err != nil {
			return err
		}
	}
	if st.currentMonth != "" && st.currentMonth != month {
		if err := sealChatLogShardLocked(ctx, st.currentMonth); err != nil {
			return err
		}
	}
	if err := ensureShardTableLocked(ctx, month); err != nil {
		return err
	}
	st.currentMonth = month
	st.ready = true
	return nil
}

func loadSealedLocked(ctx context.Context) error {
	var shards []ChatLogShard
	if err := DB.WithContext(ctx).Order("month_key DESC").Find(&shards).Error; err != nil {
		return fmt.Errorf("load chat log shards: %w", err)
	}
	chatLogShardsV.sealed = shards
	chatLogShardsV.loaded = true
	return nil
}

// EnsureChatLogShardTable 确保指定月的分表存在并接续自增。
// 不改变当前月状态、不写封存注册表；用于测试构造指定月份数据、预建分表。
func EnsureChatLogShardTable(ctx context.Context, month string) error {
	if !regexp.MustCompile(`^\d{6}$`).MatchString(month) {
		return fmt.Errorf("invalid month key: %s", month)
	}
	chatLogShardsV.mu.Lock()
	defer chatLogShardsV.mu.Unlock()
	return ensureShardTableLocked(ctx, month)
}

func ensureShardTableLocked(ctx context.Context, month string) error {	table := ChatLogShardTableName(month)
	if !DB.Migrator().HasTable(table) {
		if DB.Dialector.Name() == "mysql" {
			// MySQL 索引名按表隔离，直接用模型 AutoMigrate（索引随表内联创建）
			if err := DB.Table(table).Migrator().AutoMigrate(&ChatLog{}); err != nil {
				return fmt.Errorf("create chat log shard %s: %w", table, err)
			}
		} else {
			// sqlite 索引名全库唯一，无法跨分表复制同名索引：用从模型实时
			// 生成的无索引 DDL 模板建表（测试/开发规模无需索引兜底查询）
			ddl, err := chatLogSQLiteShardDDL()
			if err != nil {
				return fmt.Errorf("render chat log shard ddl template: %w", err)
			}
			if err := createSQLiteShardTable(table, ddl); err != nil {
				return fmt.Errorf("create chat log shard %s: %w", table, err)
			}
		}
	}
	return ensureShardIDContinuationLocked(ctx, month)
}

// sqliteShardDDL ChatLog 的 sqlite 建表 DDL 模板（不含索引），进程内生成一次。
// 从模型实时生成避免与 ChatLog 字段漂移。
var (
	sqliteShardDDLOnce sync.Once
	sqliteShardDDL     string
	sqliteShardDDLErr  error
)

func chatLogSQLiteShardDDL() (string, error) {
	sqliteShardDDLOnce.Do(func() {
		scratch, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		if err != nil {
			sqliteShardDDLErr = err
			return
		}
		if err := scratch.AutoMigrate(&ChatLog{}); err != nil {
			sqliteShardDDLErr = err
			return
		}
		var ddl string
		if err := scratch.Raw(
			"SELECT sql FROM sqlite_master WHERE type='table' AND name='chat_logs'").
			Scan(&ddl).Error; err != nil {
			sqliteShardDDLErr = err
			return
		}
		if ddl == "" {
			sqliteShardDDLErr = fmt.Errorf("empty chat_logs ddl template")
			return
		}
		sqliteShardDDL = ddl
	})
	return sqliteShardDDL, sqliteShardDDLErr
}

// createSQLiteShardDDL 用模板按表名建分表：替换 DDL 中第一个表名出现点。
func createSQLiteShardTable(table, ddl string) error {
	if i := strings.Index(ddl, "chat_logs"); i >= 0 {
		ddl = ddl[:i] + table + ddl[i+len("chat_logs"):]
		return DB.Exec(ddl).Error
	}
	return fmt.Errorf("cannot locate table name in ddl template")
}

// ensureShardIDContinuationLocked 校验并修复分表自增接续：
// next id 必须大于所有封存区间 max_id，否则新行 id 会与历史分表撞号。
// 覆盖两类场景：新建表后设置自增失败的崩溃残余、runbook 预建的空分表。
func ensureShardIDContinuationLocked(ctx context.Context, month string) error {
	table := ChatLogShardTableName(month)

	var maxSealed uint
	for _, s := range chatLogShardsV.sealed {
		if s.MaxID > maxSealed {
			maxSealed = s.MaxID
		}
	}

	var maxID int64
	if err := DB.WithContext(ctx).Table(table).
		Select("COALESCE(MAX(id), 0)").Scan(&maxID).Error; err != nil {
		return err
	}
	if maxSealed > 0 && maxID > 0 && uint(maxID) <= maxSealed {
		return fmt.Errorf("chat log shard %s has ids (max %d) overlapping sealed range (max %d), manual repair required", table, maxID, maxSealed)
	}

	next := int64(maxSealed) + 1
	if maxID >= next {
		next = maxID + 1
	}

	switch DB.Dialector.Name() {
	case "mysql":
		// 5.5 上调高 AUTO_INCREMENT 只改计数器不拷表，且此时表为空或极小
		var cur sql.NullInt64
		if err := DB.WithContext(ctx).Raw(
			"SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			table).Scan(&cur).Error; err != nil {
			return err
		}
		if !cur.Valid || cur.Int64 < next {
			if err := DB.WithContext(ctx).Exec(
				"ALTER TABLE `"+table+"` AUTO_INCREMENT = ?", next).Error; err != nil {
				return fmt.Errorf("set auto_increment for %s to %d: %w", table, next, err)
			}
		}
	default:
		// sqlite：sqlite_sequence 可安全直接写入（缺行则补）。
		// 旧实现用哑行 INSERT id=next-1 抬序列、删除后靠 AUTOINCREMENT 保留：
		// 表里已有真实数据时（任何一次带流量的重启必现）哑行会撞最大主键
		// UNIQUE，导致 ReconcileChatLogShards 失败、启动 panic。
		var seq int64
		if err := DB.WithContext(ctx).Raw(
			"SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&seq).Error; err != nil {
			seq = 0
		}
		if target := next - 1; seq < target && target > 0 {
			res := DB.WithContext(ctx).Exec(
				"UPDATE sqlite_sequence SET seq = ? WHERE name = ?", target, table)
			if res.Error != nil {
				return fmt.Errorf("bump sqlite sequence for %s: %w", table, res.Error)
			}
			if res.RowsAffected == 0 {
				if err := DB.WithContext(ctx).Exec(
					"INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)", table, target).Error; err != nil {
					return fmt.Errorf("seed sqlite sequence for %s: %w", table, err)
				}
			}
		}
	}
	return nil
}

// SealChatLogShard 封存指定月份（幂等）。边界月部分删除后重算统计也走这里。
func SealChatLogShard(ctx context.Context, month string) error {
	if !regexp.MustCompile(`^\d{6}$`).MatchString(month) {
		return fmt.Errorf("invalid month key: %s", month)
	}
	chatLogShardsV.mu.Lock()
	defer chatLogShardsV.mu.Unlock()
	return sealChatLogShardLocked(ctx, month)
}

func sealChatLogShardLocked(ctx context.Context, month string) error {
	table := ChatLogShardTableName(month)
	if !DB.Migrator().HasTable(table) {
		return nil // 当月无写入，无表可封
	}

	var row struct {
		MinID uint  `gorm:"column:min_id"`
		MaxID uint  `gorm:"column:max_id"`
		Cnt   int64 `gorm:"column:cnt"`
	}
	if err := DB.WithContext(ctx).Table(table).
		Select("COALESCE(MIN(id),0) AS min_id, COALESCE(MAX(id),0) AS max_id, COUNT(*) AS cnt").
		Scan(&row).Error; err != nil {
		return fmt.Errorf("stat chat log shard %s: %w", table, err)
	}
	if row.Cnt == 0 {
		// 空月自愈：清掉可能残留的注册行；非当前月的空分表直接删除，
		// 避免边界月被清空后留下无数据的孤儿表
		if err := DB.WithContext(ctx).Where("month_key = ?", month).
			Delete(&ChatLogShard{}).Error; err != nil {
			return err
		}
		removeSealedLocked(month)
		if month != CurrentChatLogMonthKey() {
			if err := DB.WithContext(ctx).Exec("DROP TABLE IF EXISTS `" + table + "`").Error; err != nil {
				return fmt.Errorf("drop empty chat log shard %s: %w", table, err)
			}
		}
		return nil
	}

	shard := ChatLogShard{
		MonthKey:  month,
		MinID:     row.MinID,
		MaxID:     row.MaxID,
		RowsCount: row.Cnt,
		SealedAt:  time.Now(),
	}
	if err := DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&shard).Error; err != nil {
		return fmt.Errorf("upsert chat log shard %s: %w", month, err)
	}
	upsertSealedLocked(shard)
	return nil
}

func upsertSealedLocked(shard ChatLogShard) {
	for i := range chatLogShardsV.sealed {
		if chatLogShardsV.sealed[i].MonthKey == shard.MonthKey {
			chatLogShardsV.sealed[i] = shard
			return
		}
	}
	chatLogShardsV.sealed = append(chatLogShardsV.sealed, shard)
	sort.Slice(chatLogShardsV.sealed, func(i, j int) bool {
		return chatLogShardsV.sealed[i].MonthKey > chatLogShardsV.sealed[j].MonthKey
	})
}

func removeSealedLocked(month string) {
	kept := chatLogShardsV.sealed[:0]
	for _, s := range chatLogShardsV.sealed {
		if s.MonthKey != month {
			kept = append(kept, s)
		}
	}
	chatLogShardsV.sealed = kept
}

// ChatLogShardsForWindow 返回与 [cutoff, now) 有交集的分表（新→旧），
// 当前月在最前。cutoff 零值表示不限时间（全部分表）。
// 整月早于 cutoff 的分表不返回；Boundary 标记需附加时间谓词的边界月。
func ChatLogShardsForWindow(ctx context.Context, cutoff time.Time) ([]ChatLogShardRef, error) {
	month := CurrentChatLogMonthKey()
	if err := ensureCurrentShard(ctx, month); err != nil {
		return nil, err
	}
	st := &chatLogShardsV
	st.mu.RLock()
	defer st.mu.RUnlock()

	refs := make([]ChatLogShardRef, 0, len(st.sealed)+1)
	if ref, ok := chatLogShardCoverLocked(month, cutoff); ok {
		refs = append(refs, ref)
	}
	for _, s := range st.sealed {
		if s.MonthKey == month {
			continue
		}
		if ref, ok := chatLogShardCoverLocked(s.MonthKey, cutoff); ok {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func chatLogShardCoverLocked(month string, cutoff time.Time) (ChatLogShardRef, bool) {
	start, err := chatLogMonthStart(month)
	if err != nil {
		return ChatLogShardRef{}, false
	}
	end := start.AddDate(0, 1, 0)
	if !cutoff.IsZero() && !end.After(cutoff) {
		return ChatLogShardRef{}, false
	}
	return ChatLogShardRef{
		MonthKey: month,
		Table:    ChatLogShardTableName(month),
		Boundary: !cutoff.IsZero() && cutoff.After(start),
	}, true
}

// LocateChatLogTable 按 id 定位分表：先匹配注册表封存区间，
// 未命中归当前月（该 id 可能不存在，调用方按查询结果处理）。
func LocateChatLogTable(ctx context.Context, id uint) (string, error) {
	month := CurrentChatLogMonthKey()
	if err := ensureCurrentShard(ctx, month); err != nil {
		return "", err
	}
	st := &chatLogShardsV
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, s := range st.sealed {
		if id >= s.MinID && id <= s.MaxID {
			return ChatLogShardTableName(s.MonthKey), nil
		}
	}
	return ChatLogShardTableName(st.currentMonth), nil
}

// ProbeChatLogTable 新→旧逐分表主键点查定位 id 所在表。
// 用于当前月 miss 后的兜底（跨月在途响应的 Update 等）；
// 任何分表都不含该 id 时返回 gorm.ErrRecordNotFound。
func ProbeChatLogTable(ctx context.Context, id uint) (string, error) {
	month := CurrentChatLogMonthKey()
	if err := ensureCurrentShard(ctx, month); err != nil {
		return "", err
	}
	st := &chatLogShardsV
	st.mu.RLock()
	sealed := append([]ChatLogShard(nil), st.sealed...)
	current := st.currentMonth
	st.mu.RUnlock()

	try := func(table string) (bool, error) {
		var n int64
		if err := DB.WithContext(ctx).Table(table).
			Where("id = ?", id).Limit(1).Count(&n).Error; err != nil {
			return false, err
		}
		return n > 0, nil
	}

	if current != "" {
		if ok, err := try(ChatLogShardTableName(current)); err != nil {
			return "", err
		} else if ok {
			return ChatLogShardTableName(current), nil
		}
	}
	for _, s := range sealed {
		table := ChatLogShardTableName(s.MonthKey)
		if ok, err := try(table); err != nil {
			return "", err
		} else if ok {
			return table, nil
		}
	}
	return "", gorm.ErrRecordNotFound
}

// SealedChatLogShards 返回封存月份快照（新→旧）。行数与 id 区间在封存时
// 冻结，可直接用于总数合计与清理决策。
func SealedChatLogShards() []ChatLogShard {
	st := &chatLogShardsV
	st.mu.RLock()
	defer st.mu.RUnlock()
	return append([]ChatLogShard(nil), st.sealed...)
}

// SealedMaxChatLogID 封存区间最大 id，无封存月返回 0。
// 水位线用它判断自身口径是否已跨月过期。
func SealedMaxChatLogID() uint {
	st := &chatLogShardsV
	st.mu.RLock()
	defer st.mu.RUnlock()
	if len(st.sealed) == 0 {
		return 0
	}
	return st.sealed[0].MaxID
}

// DropChatLogShard 删除指定封存月的分表与注册表行。
// 必须在事务外调用：MariaDB 5.5 的 DDL 会隐式提交所在事务。
func DropChatLogShard(ctx context.Context, month string) error {
	if !regexp.MustCompile(`^\d{6}$`).MatchString(month) {
		return fmt.Errorf("invalid month key: %s", month)
	}
	table := ChatLogShardTableName(month)
	if err := DB.WithContext(ctx).Exec("DROP TABLE IF EXISTS `" + table + "`").Error; err != nil {
		return fmt.Errorf("drop chat log shard %s: %w", table, err)
	}
	if err := DB.WithContext(ctx).Where("month_key = ?", month).
		Delete(&ChatLogShard{}).Error; err != nil {
		return fmt.Errorf("delete chat log shard registry row %s: %w", month, err)
	}
	chatLogShardsV.mu.Lock()
	removeSealedLocked(month)
	chatLogShardsV.mu.Unlock()
	return nil
}

// ReconcileChatLogShards 启动对账：注册表与实际分表双向收敛。
// 有表无行（跨月宕机未封存）→ 补封存；有行无表（DROP 后残留）→ 删行；
// 最后确保当前月分表存在且自增接续。启动后由各入口的缓存快路径接管。
func ReconcileChatLogShards(ctx context.Context) error {
	st := &chatLogShardsV
	st.mu.Lock()
	defer st.mu.Unlock()

	if err := loadSealedLocked(ctx); err != nil {
		return err
	}

	tables, err := DB.Migrator().GetTables()
	if err != nil {
		return fmt.Errorf("list tables for shard reconcile: %w", err)
	}

	current := CurrentChatLogMonthKey()
	seen := make(map[string]bool)
	for _, t := range tables {
		if !chatLogShardTableRe.MatchString(t) {
			continue
		}
		month := strings.TrimPrefix(t, chatLogShardTablePrefix)
		seen[month] = true
		if month == current {
			continue
		}
		registered := false
		for _, s := range st.sealed {
			if s.MonthKey == month {
				registered = true
				break
			}
		}
		if !registered {
			if err := sealChatLogShardLocked(ctx, month); err != nil {
				return err
			}
		}
	}

	for _, s := range append([]ChatLogShard(nil), st.sealed...) {
		if !seen[s.MonthKey] {
			if err := DB.WithContext(ctx).Where("month_key = ?", s.MonthKey).
				Delete(&ChatLogShard{}).Error; err != nil {
				return err
			}
			removeSealedLocked(s.MonthKey)
		}
	}

	return prepareCurrentShardLocked(ctx, current)
}

// ResetChatLogShardState 清空进程内分表状态缓存。测试替换 models.DB 后必须调用，
// 否则旧库的缓存（当前月、封存列表）会串进新用例。
func ResetChatLogShardState() {
	st := &chatLogShardsV
	st.mu.Lock()
	st.loaded = false
	st.ready = false
	st.currentMonth = ""
	st.sealed = nil
	st.mu.Unlock()
}
