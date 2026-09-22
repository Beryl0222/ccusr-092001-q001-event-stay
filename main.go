// 赛事停留链路协调服务入口。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/beryl0222/event-stay-orchestrator/internal/api"
	"github.com/beryl0222/event-stay-orchestrator/internal/auth"
	"github.com/beryl0222/event-stay-orchestrator/internal/store"
	"github.com/beryl0222/event-stay-orchestrator/service"
)

const serviceID = "event-stay-orchestrator"

func health() map[string]string { return map[string]string{"status": "ok", "service": serviceID} }

func main() {
	check := flag.Bool("check", false, "检查基础配置")
	addr := flag.String("addr", ":8000", "监听地址")
	dataDir := flag.String("data", "./data", "事件日志数据目录")
	tokensPath := flag.String("tokens", "./config/tokens.json", "岗位令牌表路径")
	flag.Parse()

	if *check {
		runCheck()
		return
	}

	eventLog, err := store.OpenEventLog(*dataDir)
	if err != nil {
		log.Fatalf("打开事件日志失败: %v", err)
	}
	defer eventLog.Close()

	app, err := service.New(eventLog, nil)
	if err != nil {
		log.Fatalf("应用服务初始化失败: %v", err)
	}

	var resolver *auth.Resolver
	if r, err := auth.LoadTokens(*tokensPath); err != nil {
		log.Printf("警告: 未加载岗位令牌表（%v），除观众意向与健康检查外接口将拒绝访问", err)
	} else {
		resolver = r
		log.Printf("已加载岗位令牌表: %s", *tokensPath)
	}

	srv := api.NewServer(app, resolver)
	log.Printf("%s 监听 %s（事件序号 %d）", serviceID, *addr, eventLog.Seq())
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

// runCheck 校验领域基础配置文件可读且约定齐备。
func runCheck() {
	raw, err := os.ReadFile("domain.json")
	if err != nil {
		log.Fatalf("读取 domain.json 失败: %v", err)
	}
	var doc struct {
		Project     string   `json:"项目"`
		Parties     []string `json:"参与方"`
		Events      []string `json:"事件"`
		Constraints []string `json:"约束"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Fatalf("domain.json 格式无效: %v", err)
	}
	if len(doc.Parties) == 0 || len(doc.Events) == 0 || len(doc.Constraints) == 0 {
		log.Fatal("domain.json 缺少参与方、事件或约束约定")
	}
	fmt.Println("基础检查通过")
}
