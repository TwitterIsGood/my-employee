package daily

import (
	"path/filepath"
	"testing"
	"time"

	"login-service/internal/logstore"
)

func at(day string, h int) time.Time {
	d, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		panic(err)
	}
	return d.Add(time.Duration(h) * time.Hour)
}

// 这条钉的是当前口径：同一个人登录几次就计几次。
// 它回答的是"今天有多少次登录"，不是"今天有多少人来过"——
// 改口径的时候这条会红，那是有意的：改口径就得连着这条一起改，不许悄悄变。
func TestBuildCountsLoginsNotPeople(t *testing.T) {
	c := Build([]logstore.Record{
		{UserID: "u1", At: at("2026-09-10", 9)},
		{UserID: "u1", At: at("2026-09-10", 10)},
		{UserID: "u1", At: at("2026-09-10", 11)},
		{UserID: "u2", At: at("2026-09-10", 12)},
	})
	if got := c.Days["2026-09-10"]; got != 4 {
		t.Errorf("当前口径是登录次数：3 次 + 1 次 = 4，实际 %d", got)
	}
}

func TestBuildSplitsByLocalDay(t *testing.T) {
	c := Build([]logstore.Record{
		{UserID: "u1", At: at("2026-09-10", 23)},
		{UserID: "u2", At: at("2026-09-11", 1)},
	})
	if c.Days["2026-09-10"] != 1 || c.Days["2026-09-11"] != 1 {
		t.Errorf("跨零点要分属两天: %+v", c.Days)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_cache.json")
	want := Build([]logstore.Record{{UserID: "u1", At: at("2026-09-10", 9)}})
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Days["2026-09-10"] != 1 || got.Records != 1 {
		t.Errorf("存进去又读出来不对: %+v", got)
	}
}

// 缓存是派生数据：丢了可以重算，但不该被当成"什么都没有"——
// 读不到就得让调用方知道，不能悄悄返回一个空表。
func TestLoadMissingFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("缓存不存在应该报错，而不是当成空汇总")
	}
}
