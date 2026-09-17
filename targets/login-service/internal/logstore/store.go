// Package logstore 管登录记录这一份事实源。
//
// 只有一份原始数据：一个只追加的 JSONL。别的东西（每日缓存之类）都是从它算出来的，
// 算错了可以重算，原始记录本身不重写。
package logstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Record 是一次登录。同一个人一天登录多次就有多条记录——
// "今天有多少人登录过"要去重，去重是聚合的事，不是记录的事。
type Record struct {
	UserID string    `json:"user_id"`
	At     time.Time `json:"at"`
}

// Store 是落在磁盘上的那一份日志。
type Store struct {
	Path string
}

func New(path string) Store { return Store{Path: path} }

// Append 追加一条。O_APPEND 保证并发写不会互相覆盖。
func (s Store) Append(userID string, at time.Time) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(Record{UserID: userID, At: at.UTC()})
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// ReadAll 读全部记录，按时间升序。
func (s Store) ReadAll() ([]Record, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("%s 第 %d 行不是合法记录: %w", s.Path, line, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// InDay 是落在同一个自然日里的记录。
//
// 按本地时区切日：需求方说的"每天"是墙上日历的每天，不是 UTC 的每天。
func InDay(rs []Record, day string) ([]Record, error) {
	if _, err := time.ParseInLocation("2006-01-02", day, time.Local); err != nil {
		return nil, fmt.Errorf("日期要写成 YYYY-MM-DD，收到的是 %q", day)
	}
	var out []Record
	for _, r := range rs {
		if r.At.In(time.Local).Format("2006-01-02") == day {
			out = append(out, r)
		}
	}
	return out, nil
}
