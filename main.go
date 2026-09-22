// 赛事停留链路协调服务入口。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
)

const serviceID = "event-stay-orchestrator"

func health() map[string]string { return map[string]string{"status": "ok", "service": serviceID} }

func main() {
	check := flag.Bool("check", false, "检查基础配置")
	addr := flag.String("addr", ":8000", "监听地址")
	storePath := flag.String("store", "data/eventlog.jsonl", "事件日志路径，传 :memory: 表示仅内存")
	authFile := flag.String("auth", "", "岗位令牌文件（JSON：令牌->岗位），缺省使用内置联调令牌")
	flag.Parse()

	if *check {
		if err := runCheck(*storePath); err != nil {
			fmt.Println("基础检查失败：", err)
			os.Exit(1)
		}
		fmt.Println("基础检查通过")
		return
	}

	var store Store
	if *storePath == ":memory:" {
		store = newMemStore()
	} else {
		fs, err := openFileStore(*storePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "打开事件日志失败：", err)
			os.Exit(1)
		}
		store = fs
	}

	app, err := NewApp(store, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "事件重放失败：", err)
		os.Exit(1)
	}

	tokens := defaultTokens()
	if *authFile != "" {
		tokens, err = loadTokens(*authFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "加载令牌文件失败：", err)
			os.Exit(1)
		}
	}

	mux := newMux(app, newAuthorizer(tokens))
	fmt.Printf("赛事停留链路协调服务启动，监听 %s，事件日志 %s，已重放 %d 条事件\n",
		*addr, store.Path(), len(store.Events()))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runCheck 核对 domain.json 的基础约定与事件日志完整性。
func runCheck(storePath string) error {
	raw, err := os.ReadFile("domain.json")
	if err != nil {
		return err
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	if d["项目"] != "赛事停留链路协调" {
		return fmt.Errorf("domain.json 项目名称不符")
	}
	for _, key := range []string{"参与方", "事件", "约束"} {
		arr, ok := d[key].([]any)
		if !ok || len(arr) == 0 {
			return fmt.Errorf("domain.json 基础约定不完整：%s", key)
		}
	}
	if storePath != ":memory:" {
		if _, err := os.Stat(storePath); err == nil {
			if _, err := openFileStore(storePath); err != nil {
				return fmt.Errorf("事件日志校验失败：%w", err)
			}
		}
	}
	return nil
}

func loadTokens(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	valid := map[string]bool{
		roleOperator: true, roleTraffic: true, roleVenue: true,
		roleLodging: true, roleTourism: true,
	}
	for token, role := range m {
		if token == "" || !valid[role] {
			return nil, fmt.Errorf("令牌文件含未知岗位 %q", role)
		}
	}
	return m, nil
}
