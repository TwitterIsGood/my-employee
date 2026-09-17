// Package leaks 守的是「这个仓库是公开的」这一件事。
//
// 为什么值得写成一个进程，而不是一条纪律：这一条已经犯过一次——
// `standards/resilience.md` 从建仓起就把内网代理域名写在正文里，一路推到公开仓库，
// 谁都没发现，因为**没有任何东西会在意**。「我提交前注意一下」的第一个版本，
// 恰好就是漏掉它的那一次。
//
// 所以闸门住在 harness 里，住在提交之前。规则写在文档里会腐，写在检查里不会。
//
// **这个文件自己也在扫描范围里**，所以它只写通用形状（IP、密钥模样、域名形状），
// 绝不写要防的那几个具体名字——闸门自己泄漏了，那才是最难看的一种失败。
// 实例专属的名字放 `local/leaks-deny.txt`（gitignored，见 denyPath）。
package leaks

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Finding 是一处命中。Text 里记原文是**故意**的：闸门报错时要让人一眼看见
// 是哪一行、写的是什么，不然他得自己去猜哪条规则中了。
type Finding struct {
	File string
	Line int
	What string
	Text string
}

func (f Finding) String() string {
	t := strings.TrimSpace(f.Text)
	if len(t) > 100 {
		t = t[:100] + "…"
	}
	return fmt.Sprintf("%s:%d  %s\n      %s", f.File, f.Line, f.What, t)
}

// allow 是**公开**主机名：写进公开仓库不构成泄漏，因为它们本来就是公开的。
// 往这里加东西是一次明确表态——加之前先问「这个域名公开出去有事吗」。
var allow = map[string]bool{
	"github.com":      true,
	"golang.org":      true,
	"pkg.go.dev":      true,
	"example.com":     true,
	"example.org":     true,
	"localhost":       true,
	"anthropic.com":   true,
	"openai.com":      true,
	"deepseek.com":    true,
	"json-schema.org": true,
}

// tlds 是能被当成域名结尾的后缀。**不做成"排除文件扩展名"**：
// 那是反着列，加一门新语言就要改闸门。正着列 TLD，`main.go` 自然落不进来。
//
// `sh` 和 `int` 故意不在里面：前者撞 shell 脚本的扩展名（`im.sh`），
// 后者撞 Go 的 `fs.Int` / `flag.Int`。两个都是真 TLD，但都不值得为它换来一个
// 见天喊狼来了的闸门——闸门喊多了，人就把它关了。
var tlds = map[string]bool{
	"com": true, "net": true, "org": true, "edu": true, "gov": true,
	"io": true, "co": true, "ai": true, "app": true, "dev": true,
	"cn": true, "uk": true, "us": true, "jp": true, "de": true, "fr": true,
	"top": true, "xyz": true, "info": true, "biz": true, "tv": true, "cc": true,
	"cloud": true, "tech": true, "site": true, "online": true, "space": true,
	"live": true, "internal": true, "corp": true, "lan": true,
}

var (
	reHost = regexp.MustCompile(`\b(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}\b`)
	reIPv4 = regexp.MustCompile(`\b(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])(?:\.(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])){3}\b`)
)

// secrets 是**值**的形状。这些一旦进仓库，改历史都追不回来——只有换掉。
// 名字里带 `sk-` 之类的普通字面量（比如文档里说"token 叫 sk-xxx"）不会中，
// 因为都要求后面跟足够长的随机串。
var secrets = []struct {
	name string
	re   *regexp.Regexp
}{
	{"密钥值（sk- 开头）", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)},
	{"密钥值（GitHub token）", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`)},
	{"密钥值（AWS access key）", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"密钥值（Google API key）", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"密钥值（Slack token）", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"私钥块", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"字面量 Bearer 凭据", regexp.MustCompile(`\bBearer\s+[A-Za-z0-9._-]{20,}`)},
}

// Scan 扫一个文件。file 只用来填 Finding，内容由调用方读。
func Scan(file string, src []byte) []Finding {
	var out []Finding
	sc := bufio.NewScanner(strings.NewReader(string(src)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		hit := func(what, text string) {
			out = append(out, Finding{File: file, Line: n, What: what, Text: text})
		}

		for _, s := range secrets {
			if m := s.re.FindString(line); m != "" {
				hit(s.name, m)
			}
		}
		for _, m := range reIPv4.FindAllString(line, -1) {
			if loopback(m) || m == "0.0.0.0" || m == "255.255.255.255" {
				continue
			}
			hit("IP 字面量", m)
		}
		for _, loc := range reHost.FindAllStringIndex(line, -1) {
			m := line[loc[0]:loc[1]]
			if !looksLikeHost(m) || publicHost(m) || inPath(line, loc[0]) {
				continue
			}
			hit("域名", m)
		}
	}
	return out
}

func loopback(ip string) bool { return strings.HasPrefix(ip, "127.") }

// looksLikeHost 只看结尾那个标签像不像 TLD。
// `chain.json`、`main.go`、`os.Args`、`hooks.UserPromptSubmit` 到这儿就出去了——
// 它们也是点分名字，但不是主机名。
func looksLikeHost(name string) bool {
	parts := strings.Split(strings.ToLower(strings.Trim(name, ".")), ".")
	return tlds[parts[len(parts)-1]]
}

// inPath 判断这个点分名字是路径里的一段，还是主机名。
//
// `front/im.sh` 是路径，`https://host` 是主机名。光看名字分不出来——
// 得看前面那个斜杠是不是 `://` 的一部分。
func inPath(line string, at int) bool {
	prefix := line[:at]
	if strings.Contains(prefix, "://") {
		return false
	}
	slash := strings.LastIndex(prefix, "/")
	if slash < 0 {
		return false
	}
	// 斜杠和名字之间隔了空格引号之类，说明这个斜杠说的是别处
	return !strings.ContainsAny(prefix[slash+1:], " \t\"'`()[].,;:")
}

func publicHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "."))
	if allow[h] {
		return true
	}
	// 只做一层后缀判断：`api.github.com` 这种子域按根域放行。
	parts := strings.Split(h, ".")
	root := strings.Join(parts[len(parts)-2:], ".")
	return allow[root]
}

// Deny 读实例专属的名单，一行一个。文件不存在是正常的：
// 机制进仓库，**名字待在机器上**——这正是这个包存在的理由，别把它写进这个文件里。
func Deny(path string) ([]*regexp.Regexp, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*regexp.Regexp
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile(regexp.QuoteMeta(line))
		if err != nil {
			return nil, fmt.Errorf("%s 里的 %q 编译不了: %w", path, line, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// ScanDeny 拿名单再扫一遍。命中就是命中，不区分是哪一条规则——
// 名单里写的是名字本身，**报错时不许把它打出来**，否则闸门自己就成了出口。
func ScanDeny(file string, src []byte, deny []*regexp.Regexp) []Finding {
	if len(deny) == 0 {
		return nil
	}
	var out []Finding
	sc := bufio.NewScanner(strings.NewReader(string(src)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		for _, re := range deny {
			if re.MatchString(sc.Text()) {
				out = append(out, Finding{
					File: file, Line: n,
					What: "命中本机禁名单（名字本身不回显，见 local/leaks-deny.txt）",
					Text: "该行内容已隐去",
				})
				break
			}
		}
	}
	return out
}

// Dedup 把同一行的多条命中收成一条。一行里三个 `-` 之类的重复报没有意义。
func Dedup(fs []Finding) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, f := range fs {
		k := fmt.Sprintf("%s:%d", f.File, f.Line)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}
