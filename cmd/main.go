package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"email-automation-agent/internal/agent"
	"email-automation-agent/internal/config"
)

var (
	configPath = flag.String("config", "configs/default.yaml", "Path to configuration file")
)

func main() {
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	log.Println("Configuration loaded successfully")

	// 创建代理
	automationAgent, err := agent.NewAgent(cfg, *configPath)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}

	// 启动代理
	if err := automationAgent.Start(); err != nil {
		log.Fatalf("Failed to start agent: %v", err)
	}
	log.Println("Email automation agent started")

	// 等待退出信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
	if err := automationAgent.Stop(); err != nil {
		log.Printf("Error stopping agent: %v", err)
	}

	log.Println("Email automation agent stopped")
}
