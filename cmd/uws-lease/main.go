// Command uws-lease runs the strongly-consistent coordination point used by
// remote WebTouch engines for profile single-writer leases. It never stores
// browser state; only opaque lease ownership records live here.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"uws/pkg/fingerprint"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:50152", "lease service listen address")
	root := flag.String("root", "", "lease record directory on a strongly consistent filesystem")
	tokenFile := flag.String("auth-token-file", "", "file containing the required bearer token")
	maxTTL := flag.Duration("max-ttl", 5*time.Minute, "maximum accepted lease TTL")
	tlsCert := flag.String("tls-cert", "", "TLS server certificate; required with --tls-key for a non-loopback listener")
	tlsKey := flag.String("tls-key", "", "TLS server private key; required with --tls-cert")
	flag.Parse()
	if strings.TrimSpace(*root) == "" || strings.TrimSpace(*tokenFile) == "" {
		log.Fatal("必须提供 --root 与 --auth-token-file")
	}
	if err := validateLeaseServerTransport(*listen, *tlsCert, *tlsKey); err != nil {
		log.Fatal(err)
	}
	var tlsConfig *tls.Config
	if *tlsCert != "" {
		loadedTLSConfig, err := loadLeaseTLSConfig(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatal(err)
		}
		tlsConfig = loadedTLSConfig
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		log.Fatalf("读取租约认证文件失败: %v", err)
	}
	store, err := fingerprint.OpenLeaseStore(*root)
	if err != nil {
		log.Fatalf("初始化租约存储失败: %v", err)
	}
	handler, err := fingerprint.NewLeaseHTTPHandler(fingerprint.LeaseHTTPHandlerConfig{
		Store: store, BearerToken: strings.TrimSpace(string(token)), MaxTTL: *maxTTL,
	})
	if err != nil {
		log.Fatalf("初始化租约服务失败: %v", err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", *listen, err)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	server.TLSConfig = tlsConfig
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		var serveErr error
		if *tlsCert != "" {
			serveErr = server.ServeTLS(listener, "", "")
		} else {
			serveErr = server.Serve(listener)
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("租约服务退出: %v", serveErr)
			stop()
		}
	}()
	if *tlsCert != "" {
		fmt.Printf("WebTouch 租约 HTTPS 服务已监听 %s。\n", listener.Addr())
	} else {
		fmt.Printf("WebTouch 租约服务已监听 %s（仅限本机回环）。\n", listener.Addr())
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("租约服务关闭超时: %v", err)
	}
}

// validateLeaseServerTransport keeps the bearer-token lease authority from
// becoming a remotely reachable cleartext service by configuration accident.
// A reverse proxy is still valid, but direct remote service is HTTPS-only.
func validateLeaseServerTransport(listen, certFile, keyFile string) error {
	hasCert := strings.TrimSpace(certFile) != ""
	hasKey := strings.TrimSpace(keyFile) != ""
	if hasCert != hasKey {
		return fmt.Errorf("--tls-cert 与 --tls-key 必须同时提供")
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return fmt.Errorf("--listen 必须是 host:port: %w", err)
	}
	if !hasCert && !isLoopbackListenerHost(host) {
		return fmt.Errorf("非回环 --listen 必须提供 --tls-cert 与 --tls-key；明文租约服务只允许本机回环")
	}
	return nil
}

// loadLeaseTLSConfig validates the certificate pair before opening a listener,
// so a deployment never briefly advertises a usable HTTPS service and then
// exits due to unreadable or mismatched TLS material.
func loadLeaseTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载 TLS 证书或私钥: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}, nil
}

func isLoopbackListenerHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
