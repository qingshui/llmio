package models

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

// 重启场景：当前月分表已有真实数据时，id 接续必须是无害 no-op。
// 旧实现的哑行种子（INSERT id=next-1 抬 sqlite_sequence）在表已有
// 数据时（任何一次带流量的重启必现）会撞已存在的最大主键 → UNIQUE
// 冲突 → ReconcileChatLogShards 失败 → 启动 panic。

func TestEnsureShardIDContinuationWithExistingRows(t *testing.T) {
	setupShardTestDB(t)

	month := ChatLogMonthKey(time.Now())
	table := ChatLogShardTableName(month)
	if err := ensureCurrentShard(context.Background(), month); err != nil {
		t.Fatalf("ensureCurrentShard: %v", err)
	}

	// 写入真实数据，模拟已跑过流量的当前月表
	for i := 0; i < 3; i++ {
		if err := WithChatLogShardForInsert(context.Background(), func(tx *gorm.DB, tbl string) error {
			return tx.Exec(fmt.Sprintf("INSERT INTO `%s` (name, status) VALUES (?, 'success')", tbl),
				fmt.Sprintf("r%d", i)).Error
		}); err != nil {
			t.Fatal(err)
		}
	}
	var maxID int64
	if err := DB.Table(table).Select("COALESCE(MAX(id),0)").Scan(&maxID).Error; err != nil {
		t.Fatal(err)
	}
	if maxID == 0 {
		t.Fatal("precondition: rows must exist")
	}

	// 模拟重启：清空进程内分表状态（DB 与 sqlite_sequence 保留），再次走建表+接续路径
	ResetChatLogShardState()
	if err := ensureCurrentShard(context.Background(), month); err != nil {
		t.Fatalf("re-run ensureCurrentShard with existing rows: %v", err)
	}

	// 新插入必须接续 max+1，不留空洞
	next := insertShardLog(t, "next")
	if next.ID != uint(maxID+1) {
		t.Errorf("next id = %d, want %d (max+1, no gap)", next.ID, maxID+1)
	}
}

// 序列滞后修复也不得经过哑行（直接写 sqlite_sequence）。
func TestEnsureShardIDContinuationRepairsLaggingSequence(t *testing.T) {
	setupShardTestDB(t)

	month := ChatLogMonthKey(time.Now())
	table := ChatLogShardTableName(month)
	if err := ensureCurrentShard(context.Background(), month); err != nil {
		t.Fatalf("ensureCurrentShard: %v", err)
	}
	for i := 0; i < 2; i++ {
		insertShardLog(t, fmt.Sprintf("r%d", i))
	}
	// 人为把序列拉低，模拟 sqlite_sequence 损坏/丢失
	if err := DB.Exec("UPDATE sqlite_sequence SET seq = 1 WHERE name = ?", table).Error; err != nil {
		t.Fatal(err)
	}

	// 模拟重启后走接续路径
	ResetChatLogShardState()
	if err := ensureCurrentShard(context.Background(), month); err != nil {
		t.Fatalf("ensureCurrentShard with lagging sequence: %v", err)
	}
	next := insertShardLog(t, "next")
	var maxID int64
	if err := DB.Table(table).Select("COALESCE(MAX(id),0)").Scan(&maxID).Error; err != nil {
		t.Fatal(err)
	}
	if next.ID != uint(maxID) {
		t.Errorf("next id = %d, want %d (global max)", next.ID, maxID)
	}
}

// 空新表（无 sqlite_sequence 行）的种子路径保持原行为：首插接续封存上限。
func TestEnsureShardIDContinuationFreshTableSeedsSequence(t *testing.T) {
	setupShardTestDB(t)

	// 封存一个 202610 表，抬出 sealed 上限
	sealedMonth := "202610"
	if err := EnsureChatLogShardTable(context.Background(), sealedMonth); err != nil {
		t.Fatalf("EnsureChatLogShardTable: %v", err)
	}
	if err := DB.Table(ChatLogShardTableName(sealedMonth)).Exec(
		"INSERT INTO `" + ChatLogShardTableName(sealedMonth) + "` (id, name, status) VALUES (100, 'old', 'success')").Error; err != nil {
		t.Fatal(err)
	}
	if err := sealChatLogShardLocked(context.Background(), sealedMonth); err != nil {
		t.Fatal(err)
	}

	// 全新的当前月表：首插必须从 101 接续
	month := ChatLogMonthKey(time.Now())
	if err := ensureCurrentShard(context.Background(), month); err != nil {
		t.Fatalf("ensureCurrentShard: %v", err)
	}
	first := insertShardLog(t, "first")
	if first.ID != 101 {
		t.Errorf("first id = %d, want 101 (sealed max 100 + 1, no gap)", first.ID)
	}
}
