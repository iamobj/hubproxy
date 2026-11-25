package utils

import (
	"net"
	"net/http"
	"os"
	"time"

	"hubproxy/config"
)

var (
	globalHTTPClient       *http.Client
	searchHTTPClient       *http.Client
	releaseProbeHTTPClient *http.Client
)

// InitHTTPClients 初始化HTTP客户端
func InitHTTPClients() {
	cfg := config.GetConfig()

	if p := cfg.Access.Proxy; p != "" {
		os.Setenv("HTTP_PROXY", p)
		os.Setenv("HTTPS_PROXY", p)
	}

	sharedTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
	}

	globalHTTPClient = &http.Client{
		Transport: sharedTransport,
	}

	releaseProbeHTTPClient = &http.Client{
		Timeout:   15 * time.Second,
		Transport: sharedTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	searchHTTPClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			DisableCompression:  false,
		},
	}
}

// GetGlobalHTTPClient 获取全局HTTP客户端
func GetGlobalHTTPClient() *http.Client {
	return globalHTTPClient
}

// GetReleaseProbeHTTPClient 获取不跟随重定向的Release探测HTTP客户端
func GetReleaseProbeHTTPClient() *http.Client {
	if releaseProbeHTTPClient != nil {
		return releaseProbeHTTPClient
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// GetSearchHTTPClient 获取搜索HTTP客户端
func GetSearchHTTPClient() *http.Client {
	return searchHTTPClient
}
