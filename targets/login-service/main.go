// Command login-service 是本地的登录记录服务，也是 AI 员工流水线的靶子。
//
// 它做什么：收登录事件、落成一条只追加的记录、给后台页面提供每日查询。
//
// 只有一份事实源：data/logins.jsonl。别的东西都是从它算出来的。
// 部署形态：一个进程 + 一个数据目录，换端口/换数据目录就能起第二个实例——
// 06 的两级灰度就是"起第二个实例、看观测、再切过来"。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"login-service/internal/api"
	"login-service/internal/daily"
	"login-service/internal/logstore"
)

// runReindex 把每日汇总从原始记录重算一遍。
//
// 这一步是**按当前代码**算的，所以代码换了口径而没跑这一步，
// 页面上就还是老数字——缓存是派生数据，它的正确性取决于算它的那段代码。
func runReindex(store logstore.Store, out string) error {
	rs, err := store.ReadAll()
	if err != nil {
		return err
	}
	c := daily.Build(rs)
	c.Source = store.Path
	if err := c.Save(out); err != nil {
		return err
	}
	fmt.Printf("已重建 %s：%d 条原始记录，%d 天\n", out, c.Records, len(c.Days))
	return nil
}

func main() {
	addr := flag.String("addr", ":8080", "监听地址")
	data := flag.String("data", "data", "数据目录")
	version := flag.String("version", "dev", "版本号，写进 /healthz 便于确认现在跑的是哪一版")
	reindex := flag.Bool("reindex", false, "按当前代码把每日汇总从原始记录重算一遍，然后退出")
	flag.Parse()

	store := logstore.New(*data + "/logins.jsonl")
	if *reindex {
		if err := runReindex(store, *data+"/daily_cache.json"); err != nil {
			fmt.Fprintln(os.Stderr, "重建失败:", err)
			os.Exit(1)
		}
		return
	}

	srv := &api.Server{Store: store, CachePath: *data + "/daily_cache.json", Version: *version}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           api.WithVersion(*version, srv.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "监听失败:", err)
		os.Exit(1)
	}
	fmt.Printf("login-service %s 起在 %s，数据在 %s\n", *version, ln.Addr(), *data)

	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "服务退出:", err)
			os.Exit(1)
		}
	}()

	// 收到 TERM 就停——灰度切流量时靠这个干净地停掉旧实例。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	fmt.Println("已停止")
}
