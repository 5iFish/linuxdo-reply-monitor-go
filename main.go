package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	c, err := loadConfig()
	if err != nil {
		log.Fatalf("配置无效: %v", err)
	}
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		log.Fatal(err)
	}
	absolute, err := filepath.Abs(c.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("数据目录: %s", absolute)
	db, err := openDB(c.DataDir)
	if err != nil {
		log.Fatalf("SQLite 打开失败: %v", err)
	}
	defer db.Close()
	if err := seedRule(db, c); err != nil {
		log.Fatal(err)
	}
	discourse := newDiscourseClient(c)
	worker := &Worker{DB: db, Config: c, Discourse: discourse}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go worker.Run(ctx)
	web := newWebApp(db, c, discourse)
	srv := &http.Server{Addr: c.Listen, Handler: web.handler(), ReadHeaderTimeout: 8 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("Web 管理页面监听 %s", c.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
