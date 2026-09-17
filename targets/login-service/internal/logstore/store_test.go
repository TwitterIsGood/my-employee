package logstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tmp(t *testing.T) Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "logins.jsonl"))
}

func TestAppendThenReadBack(t *testing.T) {
	s := tmp(t)
	at := time.Date(2026, 9, 10, 9, 0, 0, 0, time.Local)
	if err := s.Append("u1", at); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("u2", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UserID != "u1" || got[1].UserID != "u2" {
		t.Fatalf("两条记录读回来不对: %+v", got)
	}
}

// 没有数据文件不是错误：新起的实例还没有人登录过。
func TestMissingFileIsEmptyNotError(t *testing.T) {
	got, err := tmp(t).ReadAll()
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应该是空: %+v", got)
	}
}

func TestReadAllReportsTheBadLine(t *testing.T) {
	s := tmp(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("{\"user_id\":\"u1\",\"at\":\"2026-09-10T09:00:00Z\"}\n坏行\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAll(); err == nil {
		t.Error("坏行应该报错，并指出是第几行")
	}
}

// 按本地时区切日：需求方说的"每天"是墙上日历的每天，不是 UTC 的每天。
func TestInDaySplitsOnLocalCalendar(t *testing.T) {
	late := time.Date(2026, 9, 10, 23, 30, 0, 0, time.Local)
	early := time.Date(2026, 9, 11, 0, 30, 0, 0, time.Local)
	rs := []Record{{UserID: "u1", At: late}, {UserID: "u2", At: early}}

	day10, err := InDay(rs, "2026-09-10")
	if err != nil {
		t.Fatal(err)
	}
	day11, _ := InDay(rs, "2026-09-11")

	if len(day10) != 1 || len(day11) != 1 {
		t.Fatalf("跨零点应该分属两天: 10 号 %d 条, 11 号 %d 条", len(day10), len(day11))
	}
}

func TestInDayRejectsBadDate(t *testing.T) {
	if _, err := InDay(nil, "9月10日"); err == nil {
		t.Error("日期格式不对应该报错，而不是当成空结果")
	}
}
