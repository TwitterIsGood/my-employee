// Package spec 守的是「阶段 spec 树」自己的完整性。
//
// 为什么值得写：阶段契约（职责/入口/出口/闸门/驳回权/证据/反例）如果只是文档里
// 的一句话，第一个赶时间的人就会少写一节，然后"入口条件就是驳回权的依据"
// 这条机制就静默失效了。跟 delivery.md 里"写不出谓词的准则视为未落地"是同一个道理。
package spec

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var requiredSections = []string{
	"## 职责",
	"## 入口条件",
	"## 出口条件",
	"## 专属规范",
	"## 闸门",
	"## 驳回权",
	"## 证据",
	"## 反例",
	"## 契约（机器可读）",
}

func stagesDir() string {
	return filepath.Join("..", "..", "standards", "stages")
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	return string(b)
}

// section 取某个二级标题下的正文，直到下一个二级标题。
func section(doc, heading string) string {
	i := strings.Index(doc, heading)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func numberedStages(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stagesDir(), "[0-9][0-9]-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(matches)
	return matches
}

func TestStageTreeIsComplete(t *testing.T) {
	stages := numberedStages(t)
	if len(stages) != 7 {
		t.Fatalf("阶段文件应为 7 个，实际 %d 个: %v", len(stages), stages)
	}
	for i, p := range stages {
		want := regexp.MustCompile(`^(\d\d)-`).FindStringSubmatch(filepath.Base(p))
		if got := want[1]; got != pad(i+1) {
			t.Errorf("编号不连续: 第 %d 个文件是 %s，应为 %s 开头", i+1, filepath.Base(p), pad(i+1))
		}
	}
}

func pad(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func TestEveryStageHasFullContract(t *testing.T) {
	for _, p := range numberedStages(t) {
		doc := read(t, p)
		name := filepath.Base(p)
		for _, s := range requiredSections {
			if !strings.Contains(doc, s) {
				t.Errorf("%s 缺小节 %q——契约不完整的阶段不算落地", name, s)
			}
		}
		if !strings.Contains(doc, "[闸门]") {
			t.Errorf("%s 没有任何 [闸门] 谓词", name)
		}
		// 驳回权必须是一张有数据的表，不是空标题
		rej := section(doc, "## 驳回权")
		if strings.Count(rej, "\n|") < 3 {
			t.Errorf("%s 的驳回权没有实际条目", name)
		}
		// 反例要么有 ❌，要么明确说无
		if ex := section(doc, "## 反例"); !strings.Contains(ex, "❌") {
			t.Errorf("%s 的反例小节没有任何具体反例", name)
		}
	}
}

// 阶段之间必须首尾相接：每个阶段的入口条件要指向它的上游。
// 链条断在哪一环，测试就在哪一环失败。
func TestPipelineChainIsConnected(t *testing.T) {
	for i, p := range numberedStages(t) {
		if i == 0 {
			continue // 01 的上游是人，没有上游阶段
		}
		doc := read(t, p)
		upstream := pad(i) // 上一个阶段的编号
		if !strings.Contains(section(doc, "## 入口条件"), upstream) {
			t.Errorf("%s 的入口条件没有指向上游阶段 %s——链条断了",
				filepath.Base(p), upstream)
		}
	}
}

func TestIndexLinksEveryStage(t *testing.T) {
	idx := read(t, filepath.Join(stagesDir(), "index.md"))
	for _, p := range numberedStages(t) {
		name := filepath.Base(p)
		if !strings.Contains(idx, name) {
			t.Errorf("index.md 没有登记 %s", name)
		}
	}
}

// 模板自己要完整，否则照着它写的阶段都会缺东西。
func TestTemplateHasFullContract(t *testing.T) {
	doc := read(t, filepath.Join(stagesDir(), "_template.md"))
	for _, s := range requiredSections {
		if !strings.Contains(doc, s) {
			t.Errorf("_template.md 缺小节 %q", s)
		}
	}
}
