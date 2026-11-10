package service

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/samber/lo"
	"gorm.io/gorm"
	"llmio/consts"
	"llmio/models"
)

func setupRoutingDB(t *testing.T) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&models.Model{}, &models.ModelWithProvider{}, &models.Provider{}, &models.ChatLog{}); err != nil {
		t.Fatal(err)
	}
	old := models.DB
	models.DB = db
	t.Cleanup(func() {
		sqlDB.Close()
		models.DB = old
	})
}

func seedProvider(t *testing.T, id uint, typ string) {
	t.Helper()
	p := models.Provider{Name: "p" + strconv.Itoa(int(id)), Type: typ}
	p.ID = id
	if err := models.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
}

func seedAssoc(t *testing.T, modelID, providerID uint, limit int, toolCall, structuredOutput, image bool) {
	t.Helper()
	mp := models.ModelWithProvider{
		ModelID:          modelID,
		ProviderID:       providerID,
		ProviderModel:    "up",
		ToolCall:         lo.ToPtr(toolCall),
		StructuredOutput: lo.ToPtr(structuredOutput),
		Image:            lo.ToPtr(image),
		Status:           lo.ToPtr(true),
		MaxInputTokens:   limit,
	}
	if err := models.DB.Create(&mp).Error; err != nil {
		t.Fatal(err)
	}
}

func providerIDs(mps []models.ModelWithProvider) []uint {
	ids := lo.Map(mps, func(mp models.ModelWithProvider, _ int) uint { return mp.ProviderID })
	slices.Sort(ids)
	return ids
}

// 模拟 GLM-5.3：两个 500K 不支持图片的关联 (1,2) 和一个 1M 支持图片的关联 (3)。
func TestFindModelWithProvidersByLengthAndImage(t *testing.T) {
	setupRoutingDB(t)
	for _, id := range []uint{1, 2, 3} {
		seedProvider(t, id, consts.StyleAnthropic)
	}
	seedProvider(t, 4, consts.StyleOpenAI)
	seedAssoc(t, 1, 4, 500000, true, true, false)
	seedAssoc(t, 1, 1, 500000, true, true, false)
	seedAssoc(t, 1, 2, 500000, true, true, false)
	seedAssoc(t, 1, 3, 1000000, true, true, true)

	tests := []struct {
		name   string
		before Before
		want   []uint
	}{
		{"short text uses all", Before{toolCall: true, structuredOutput: true, inputTokens: 1000}, []uint{1, 2, 3}},
		{"exactly at limit still fits", Before{toolCall: true, structuredOutput: true, inputTokens: 500000}, []uint{1, 2, 3}},
		{"image goes to image-capable", Before{toolCall: true, structuredOutput: true, image: true, inputTokens: 1000}, []uint{3}},
		{"over 500K goes to 1M", Before{toolCall: true, structuredOutput: true, inputTokens: 600000}, []uint{3}},
		{"over 500K with image", Before{toolCall: true, structuredOutput: true, image: true, inputTokens: 600000}, []uint{3}},
		{"over 1M matches nothing", Before{toolCall: true, structuredOutput: true, inputTokens: 1200000}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findModelWithProviders(context.Background(), 1, consts.StyleAnthropic, tt.before)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(providerIDs(got), tt.want) {
				t.Errorf("providers = %v, want %v", providerIDs(got), tt.want)
			}
		})
	}
}

func TestFindModelWithProvidersOpenAIStyle(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 3, consts.StyleAnthropic)
	seedProvider(t, 4, consts.StyleOpenAI)
	seedAssoc(t, 1, 3, 1000000, true, true, true)
	seedAssoc(t, 1, 4, 500000, true, true, false)

	before := Before{toolCall: true, structuredOutput: true, inputTokens: 1000}
	got, err := findModelWithProviders(context.Background(), 1, consts.StyleOpenAI, before)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(providerIDs(got), []uint{4}) {
		t.Errorf("short text providers = %v, want [4]", providerIDs(got))
	}

	// 图片请求：协议内没有支持图片的关联时，沿用放宽后的 openai 关联，而不是选中其他协议的关联
	before.image = true
	got, err = findModelWithProviders(context.Background(), 1, consts.StyleOpenAI, before)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(providerIDs(got), []uint{4}) {
		t.Errorf("image providers = %v, want [4]", providerIDs(got))
	}

	// 超过 openai 关联上限：协议内没有可用关联
	before.image = false
	before.inputTokens = 600000
	got, err = findModelWithProviders(context.Background(), 1, consts.StyleOpenAI, before)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("long input providers = %v, want none", providerIDs(got))
	}
}

func TestFindModelWithProvidersRelaxation(t *testing.T) {
	setupRoutingDB(t)
	for _, id := range []uint{10, 11, 20} {
		seedProvider(t, id, consts.StyleAnthropic)
	}
	// 模型 2：图片关联未声明 structured_output，非图片关联声明了全部能力
	seedAssoc(t, 2, 10, 0, true, false, true)
	seedAssoc(t, 2, 11, 0, true, true, false)
	// 模型 3：没有任何图片关联，且上限 1000
	seedAssoc(t, 3, 20, 1000, true, true, false)

	t.Run("relaxes structured_output but keeps image", func(t *testing.T) {
		got, err := findModelWithProviders(context.Background(), 2, consts.StyleAnthropic, Before{toolCall: true, structuredOutput: true, image: true, inputTokens: 100})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(providerIDs(got), []uint{10}) {
			t.Errorf("providers = %v, want [10]", providerIDs(got))
		}
	})
	t.Run("no image association falls back to all enabled", func(t *testing.T) {
		got, err := findModelWithProviders(context.Background(), 3, consts.StyleAnthropic, Before{toolCall: true, structuredOutput: true, image: true, inputTokens: 100})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(providerIDs(got), []uint{20}) {
			t.Errorf("providers = %v, want [20]", providerIDs(got))
		}
	})
	t.Run("length is never relaxed", func(t *testing.T) {
		got, err := findModelWithProviders(context.Background(), 3, consts.StyleAnthropic, Before{toolCall: true, structuredOutput: true, image: true, inputTokens: 5000})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("providers = %v, want none", providerIDs(got))
		}
	})
}

func TestProvidersWithMetaPromptTooLong(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	m := models.Model{Name: "m-long"}
	if err := models.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	seedAssoc(t, m.ID, 1, 1000, false, false, false)
	seedAssoc(t, m.ID, 2, 2000, false, false, false)

	_, err := ProvidersWithMetaBymodelsName(context.Background(), consts.StyleAnthropic, Before{Model: "m-long", inputTokens: 5000}, 0)
	var tooLong *PromptTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v, want PromptTooLongError", err)
	}
	if tooLong.Estimated != 5000 || tooLong.Max != 2000 {
		t.Errorf("got estimated=%d max=%d, want 5000/2000", tooLong.Estimated, tooLong.Max)
	}

	empty := models.Model{Name: "m-empty"}
	if err := models.DB.Create(&empty).Error; err != nil {
		t.Fatal(err)
	}
	_, err = ProvidersWithMetaBymodelsName(context.Background(), consts.StyleAnthropic, Before{Model: "m-empty", inputTokens: 5000}, 0)
	if err == nil || errors.As(err, &tooLong) {
		t.Errorf("err = %v, want plain not-provider error", err)
	}
}
