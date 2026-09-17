// Package daily 把原始登录记录算成"每天一个数字"，落成一份缓存。
//
// 为什么要缓存：页面要求打开就能看到数，而原始记录会一直长。缓存是**派生数据**，
// 丢了、错了都能从 logins.jsonl 重算回来——所以重建它永远是安全的，
// 危险的是"代码已经换了、缓存还是按老口径算的"这种半截状态。
package daily

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"login-service/internal/logstore"
)

// Cache 是某一天 -> 一个数字。
//
// Source 和 Records 记的是"这份缓存是从哪一份原始数据算出来的"：
// 没有它，就没法判断眼前这个数字是新的还是上次留下的。
type Cache struct {
	Days    map[string]int `json:"days"`
	BuiltAt time.Time      `json:"built_at"`
	Source  string         `json:"source"`
	Records int            `json:"records"`
}

// Build 从原始记录算一遍。
//
// 口径是**当天的登录次数**：同一个人当天登录 5 次就计 5。
// 这个数回答的是"今天有多少次登录"，不是"今天有多少人来过"。
func Build(rs []logstore.Record) Cache {
	days := map[string]int{}
	for _, r := range rs {
		days[r.At.In(time.Local).Format("2006-01-02")]++
	}
	return Cache{
		Days:    days,
		BuiltAt: time.Now(),
		Records: len(rs),
	}
}

func (c Cache) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func Load(path string) (Cache, error) {
	var c Cache
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s 不是合法缓存: %w", path, err)
	}
	if c.Days == nil {
		c.Days = map[string]int{}
	}
	return c, nil
}
