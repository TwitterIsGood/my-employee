package leaks

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 样例**从零件拼出来**，不写全。
//
// 因为这个测试文件自己也在下面那条全库扫描的范围里——写全了，闸门会把自己判成泄漏。
// 这不是绕开检查，这正是检查在起作用：它不认"这是测试数据"这种理由，
// 而发布出去的仓库同样不认。
func badSamples() []struct{ name, text string } {
	ip := strings.Join([]string{"10", "1", "2", "3"}, ".")
	host := strings.Join([]string{"proxy", "corp", "internal"}, ".")
	return []struct{ name, text string }{
		{"内部域名", "出口指向 `" + host + "`，502 烧掉 402 秒"},
		{"私网 IP", "服务监听 " + ip + ":<端口>"},
		{"密钥值", `TOK="sk-` + strings.Repeat("x", 40) + `"`},
		{"GitHub token", "token=ghp_" + strings.Repeat("A", 36)},
		{"AWS access key", "AKIA" + strings.Repeat("B", 16)},
		{"私钥块", "-----BEGIN " + "RSA PRIVATE KEY" + "-----"},
		{"写死的 Bearer", "Authorization: Bearer " + strings.Repeat("q", 30)},
		{"公网 IP", "把 " + strings.Join([]string{"8", "8", "8", "8"}, ".") + " 指过去"},
	}
}

func TestScanCatchesLeaks(t *testing.T) {
	for _, s := range badSamples() {
		if got := Scan("x.md", []byte(s.text)); len(got) == 0 {
			t.Errorf("%s 没被拦下，内容: %s", s.name, s.text)
		}
	}
}

func TestScanLetsNormalContentThrough(t *testing.T) {
	// 这些都是这仓库里真实出现的东西，闸门不许对着它们喊。
	ok := []string{
		"看 [`front/im.sh`](front/im.sh) 和 `cmd/egress-listener/main.go`",
		"```\nBASE=${MULTICA_BASE:-http://localhost:13000}\nTOK=\"${MULTICA_TOKEN:-$(...)}\"\n```",
		"curl http://127.0.0.1:18444/v1/messages",
		"见 https://github.com/TwitterIsGood/my-employee 与 pkg.go.dev 的文档",
		"`targets/login-service/data/logins.jsonl`、`go.mod`、`standards/stages/07-regression.md`",
		"5.6.5 之类的版本号，2026-09-17 这样的日期",
		"把 `user_id` 长 2000000 的请求发出去",
	}
	for _, s := range ok {
		if got := Scan("x.md", []byte(s)); len(got) > 0 {
			t.Errorf("正常内容被误判: %s\n  -> %v", s, got)
		}
	}
}

// Deny 名单报错时**不许把名字打出来**——闸门自己成了出口，那是最难看的一种失败。
func TestDeniedNamesAreNotEchoed(t *testing.T) {
	secretName := strings.Join([]string{"cpa", "internal-host", "top"}, ".")
	f := ScanDeny("x.md", []byte("出口是 "+secretName), mustDeny(t, secretName))
	if len(f) == 0 {
		t.Fatal("禁名单没命中")
	}
	if strings.Contains(f[0].Text, secretName) {
		t.Errorf("闸门把禁名单里的名字原样打出来了: %q", f[0].Text)
	}
}

func mustDeny(t *testing.T, name string) []*regexp.Regexp {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "deny.txt")
	if err := os.WriteFile(p, []byte("# 注释行\n\n"+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	re, err := Deny(p)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

// 全库扫一遍：**跟踪的文件**才算数。gitignore 掉的东西本来就不进仓库，
// 对它们喊是噪音——`local/` 里正住着含凭据的 settings 文件。
func TestNoLeaksInTrackedFiles(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files 跑不动: %v", err)
	}
	files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(files) < 10 {
		t.Fatalf("只列到 %d 个文件，八成是跑错地方了", len(files))
	}

	deny, err := Deny(filepath.Join(root, "local", "leaks-deny.txt"))
	if err != nil {
		t.Fatalf("本机禁名单读不了: %v", err)
	}

	var found []Finding
	for _, rel := range files {
		if rel == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("读不到 %s: %v", rel, err)
			continue
		}
		if bytes.IndexByte(b, 0) >= 0 {
			continue // 二进制
		}
		found = append(found, Scan(rel, b)...)
		found = append(found, ScanDeny(rel, b, deny)...)
	}

	for _, f := range Dedup(found) {
		t.Errorf("这个仓库是公开的，这一行不该在里面:\n  %s", f)
	}
	if len(found) > 0 {
		t.Log("教训该留下，端点标识不该。改写成占位符（见 standards/leaks.md），" +
			"或者——如果它本来就是公开的——加进 leaks.go 的 allow 表并说明理由。")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("找不到仓库根: %v", err)
	}
	return strings.TrimSpace(string(out))
}
